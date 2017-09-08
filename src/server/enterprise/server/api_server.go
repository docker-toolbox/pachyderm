package server

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/gogo/protobuf/types"
	logrus "github.com/sirupsen/logrus"
	"golang.org/x/net/context"

	"github.com/pachyderm/pachyderm/src/client"
	ec "github.com/pachyderm/pachyderm/src/client/enterprise"
	"github.com/pachyderm/pachyderm/src/server/pkg/backoff"
	col "github.com/pachyderm/pachyderm/src/server/pkg/collection"
	"github.com/pachyderm/pachyderm/src/server/pkg/log"
	"github.com/pachyderm/pachyderm/src/server/pkg/watch"
)

const (
	enterprisePrefix = "/enterprise"

	publicKey = `-----BEGIN PUBLIC KEY-----
MIICIjANBgkqhkiG9w0BAQEFAAOCAg8AMIICCgKCAgEAoaPoEfv5RcVUbCuWNnOB
WtLHzcyQSe4SbtGGQom/X27iq/7s8dcebSsCd2cwYoyKihEQ5OlaghrhcxTTV5AN
39O6S0YnWjt/+4PWQQP3NpcEhqWj8RLPJtYq+JNrqlyjxBlca7vDcFSTa6iCqXay
iVD2OyTbWrD6KZ/YTSmSY8mY2qdYvHyp3Ue5ueH3rSkKRUjo4Jyjf59PntZD884P
yb9kC+weh/1KlbDQ4aV0U9p6DSBkW7dinOQj7a1/ikDoA9Nebnrkb1FF9Hr2+utO
We4e4yOViDzAP9hhQiBhOVR0F6wJF5i+NfuLit4tk5ViboogEZqIyuakTD6abSFg
UPqBTDDG0UsVqjnU5ysJ1DKQqALnOrxEKZoVXtH80/m7kgmeY3VDHCFt+WCSdaSq
1w8SoIpJAZPJpKlDjMxe+NqsX2qUODQ2KNkqfEqFtyUNZzfS9o9pEg/KJzDuDclM
oMQr1BG8vc3msX4UiGQPkohznwlCSGWf62IkSS6P8hQRCBKGRS5yGjmT3J+/chZw
Je46y8zNLV7t2pOL6UemdmDjTaMCt0YBc1FmG2eUipAWcHJWEHgQm2Yz6QjtBgvt
jFqnYeiDwdxU7CQD3oF9H+uVHqz8Jmmf9BxY9PhlMSUGPUsTpZ717ysL0UrBhQhW
xYp8vpeQ3by9WxPBE/WrxN8CAwEAAQ==
-----END PUBLIC KEY-----
`

	// enterpriseTokenKey is the constant key we use that maps to an Enterprise
	// token that a user has given us. This is what we check to know if a
	// Pachyderm cluster supports enterprise features
	enterpriseTokenKey = "token"
)

type apiServer struct {
	pachLogger log.Logger
	etcdClient *etcd.Client

	// enterpriseState is a cached timestamp, indicating when the current
	// Pachyderm Enterprise token will expire (or 0 if there is no Pachyderm
	// Enterprise token
	enterpriseExpiration atomic.Value

	// enterpriseToken is a collection containing at most one Pachyderm enterprise
	// token
	enterpriseToken col.Collection
}

func (a *apiServer) LogReq(request interface{}) {
	a.pachLogger.Log(request, nil, nil, 0)
}

// NewEnterpriseServer returns an implementation of ec.APIServer.
func NewEnterpriseServer(etcdAddress string, etcdPrefix string) (ec.APIServer, error) {
	etcdClient, err := etcd.New(etcd.Config{
		Endpoints:   []string{etcdAddress},
		DialOptions: client.EtcdDialOptions(),
	})
	if err != nil {
		return nil, fmt.Errorf("error constructing etcdClient: %s", err.Error())
	}

	s := &apiServer{
		pachLogger: log.NewLogger("enterprise.API"),
		etcdClient: etcdClient,
		enterpriseToken: col.NewCollection(
			etcdClient,
			etcdPrefix, // only one collection--no extra prefix needed
			nil,
			&types.Timestamp{},
			nil,
		),
	}
	s.enterpriseExpiration.Store(time.Time{})
	go s.watchEnterpriseToken(etcdPrefix)
	return s, nil
}

func (a *apiServer) watchEnterpriseToken(etcdPrefix string) {
	backoff.RetryNotify(func() error {
		// Watch for incoming enterprise tokens
		watcher, err := a.enterpriseToken.ReadOnly(context.Background()).Watch()
		if err != nil {
			return err
		}
		defer watcher.Close()
		for {
			ev, ok := <-watcher.Watch()
			if !ok {
				return errors.New("admin watch closed unexpectedly")
			}

			// Parse event data and potentially update adminCache
			switch ev.Type {
			case watch.EventPut:
				var key string
				var record ec.EnterpriseRecord
				ev.Unmarshal(&key, &record)
				expiration, err := types.TimestampFromProto(record.Expires)
				if err != nil {
					return fmt.Errorf("could not parse expiration timestamp: %s", err.Error())
				}
				a.enterpriseExpiration.Store(expiration)
			case watch.EventDelete:
				// This should only occur if the etcd value is deleted via the etcd API,
				// but that does occur during testing
				a.enterpriseExpiration.Store(time.Time{})
			case watch.EventError:
				return ev.Err
			}
		}
	}, backoff.NewInfiniteBackOff(), func(err error, d time.Duration) error {
		logrus.Printf("error from activation check: %v; retrying in %v", err, d)
		return nil
	})
}

type activationCode struct {
	Token     string
	Signature string
}

// token is used to parse a JSON object generated by Pachyderm Inc's enterprise
// token tool. Note that if you want to change this struct, you'll have to
// change the enterprise token tool and potentially generate new tokens for all
// of Pachyderm's customers (if you're changing or removing a field).
type token struct {
	Expiry string
}

// validateActivationCode checks the validity of an activation code
func validateActivationCode(code string) (expiration time.Time, err error) {
	// Parse the public key.  If these steps fail, something is seriously
	// wrong and we should crash the service by panicking.
	block, _ := pem.Decode([]byte(publicKey))
	if block == nil {
		return time.Time{}, fmt.Errorf("failed to pem decode public key")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse DER encoded public key: %s", err.Error())
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return time.Time{}, fmt.Errorf("public key isn't an RSA key")
	}

	// Decode the base64-encoded activation code
	decodedActivationCode, err := base64.StdEncoding.DecodeString(code)
	if err != nil {
		return time.Time{}, fmt.Errorf("activation code is not base64 encoded")
	}
	activationCode := &activationCode{}
	if err := json.Unmarshal(decodedActivationCode, &activationCode); err != nil {
		return time.Time{}, fmt.Errorf("activation code is not valid JSON")
	}

	// Decode the signature
	decodedSignature, err := base64.StdEncoding.DecodeString(activationCode.Signature)
	if err != nil {
		return time.Time{}, fmt.Errorf("signature is not base64 encoded")
	}

	// Compute the sha256 checksum of the token
	hashedToken := sha256.Sum256([]byte(activationCode.Token))

	// Verify that the signature is valid
	if err := rsa.VerifyPKCS1v15(rsaPub, crypto.SHA256, hashedToken[:], decodedSignature); err != nil {
		return time.Time{}, fmt.Errorf("invalid signature in activation code")
	}

	// Unmarshal the token
	token := token{}
	if err := json.Unmarshal([]byte(activationCode.Token), &token); err != nil {
		return time.Time{}, fmt.Errorf("token is not valid JSON")
	}

	// Parse the expiration
	expiration, err = time.Parse(time.RFC3339, token.Expiry)
	if err != nil {
		return time.Time{}, fmt.Errorf("expiration is not valid ISO 8601 string")
	}
	// Check that the activation code has not expired
	if time.Now().After(expiration) {
		return time.Time{}, fmt.Errorf("the activation code has expired")
	}
	return expiration, nil
}

// Activate implements the Activate RPC
func (a *apiServer) Activate(ctx context.Context, req *ec.ActivateRequest) (resp *ec.ActivateResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.pachLogger.Log(req, resp, retErr, time.Since(start)) }(time.Now())

	// Validate the activation code
	expiration, err := validateActivationCode(req.ActivationCode)
	if err != nil {
		return nil, fmt.Errorf("error validating activation code: %s", err.Error())
	}
	// Allow request to override expiration in the activation code, for testing
	if req.Expires != nil {
		customExpiration, err := types.TimestampFromProto(req.Expires)
		if err == nil && expiration.After(customExpiration) {
			expiration = customExpiration
		}
	}
	if _, err := col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
		e := a.enterpriseToken.ReadWrite(stm)
		expirationProto, err := types.TimestampProto(expiration)
		if err != nil {
			return err
		}
		// blind write
		e.Put(enterpriseTokenKey, &ec.EnterpriseRecord{
			ActivationCode: req.ActivationCode,
			Expires:        expirationProto,
		})
		return nil
	}); err != nil {
		return nil, err
	}
	return &ec.ActivateResponse{}, nil
}

// GetState returns the current state of the cluster's Pachyderm Enterprise key (ACTIVE, EXPIRED, or NONE)
func (a *apiServer) GetState(ctx context.Context, req *ec.GetStateRequest) (resp *ec.GetStateResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.pachLogger.Log(req, resp, retErr, time.Since(start)) }(time.Now())

	expiration, ok := a.enterpriseExpiration.Load().(time.Time)
	if !ok {
		return nil, fmt.Errorf("could not retrieve enterprise expiration time")
	}
	if expiration.IsZero() {
		return &ec.GetStateResponse{State: ec.State_NONE}, nil
	}
	if time.Now().After(expiration) {
		return &ec.GetStateResponse{State: ec.State_EXPIRED}, nil
	}
	return &ec.GetStateResponse{State: ec.State_ACTIVE}, nil
}