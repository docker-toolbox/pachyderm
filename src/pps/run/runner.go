package run

import (
	"fmt"

	"github.com/pachyderm/pachyderm/src/common"
	"github.com/pachyderm/pachyderm/src/log"
	"github.com/pachyderm/pachyderm/src/pps"
	"github.com/pachyderm/pachyderm/src/pps/container"
	"github.com/pachyderm/pachyderm/src/pps/graph"
	"github.com/pachyderm/pachyderm/src/pps/source"
	"github.com/pachyderm/pachyderm/src/pps/store"
)

type runner struct {
	sourcer         source.Sourcer
	grapher         graph.Grapher
	containerClient container.Client
	storeClient     store.Client
}

func newRunner(
	sourcer source.Sourcer,
	grapher graph.Grapher,
	containerClient container.Client,
	storeClient store.Client,
) *runner {
	return &runner{
		sourcer,
		grapher,
		containerClient,
		storeClient,
	}
}

func (r *runner) Start(pipelineSource *pps.PipelineSource) (string, error) {
	dirPath, pipeline, err := r.sourcer.GetDirPathAndPipeline(pipelineSource)
	if err != nil {
		return "", err
	}
	pipelineRunID := common.NewUUID()
	if err := r.storeClient.AddPipelineRun(
		pipelineRunID,
		pipelineSource,
		pipeline,
	); err != nil {
		return "", err
	}
	log.Printf("%v %s %v\n", dirPath, pipelineRunID, pipeline)
	nameToNode := pps.GetNameToNode(pipeline)
	nameToNodeInfo, err := graph.GetNameToNodeInfo(nameToNode)
	if err != nil {
		return "", err
	}
	nameToNodeFunc := make(map[string]func() error)
	for name := range nameToNode {
		name := name
		nameToNodeFunc[name] = func() error {
			log.Printf("RUNNING %s\n", name)
			return nil
		}
	}
	run, err := r.grapher.Build(
		&dummyNodeErrorRecorder{},
		nameToNodeInfo,
		nameToNodeFunc,
	)
	if err != nil {
		return "", err
	}
	go run.Do()
	if err := r.storeClient.AddPipelineRunStatus(
		pipelineRunID,
		pps.PipelineRunStatusType_PIPELINE_RUN_STATUS_TYPE_STARTED,
	); err != nil {
		return "", err
	}
	return pipelineRunID, nil
}

func (r *runner) getNodeFunc(
	name string,
	node *pps.Node,
	nameToDockerService map[string]*pps.DockerService,
	numContainers int,
) (func() error, error) {
	dockerService, ok := nameToDockerService[node.Service]
	if !ok {
		return nil, fmt.Errorf("no service for name %s", node.Service)
	}
	if dockerService.Build != "" || dockerService.Dockerfile != "" {
		return nil, fmt.Errorf("build/dockerfile not supported yet")
	}
	return func() (retErr error) {
		if err := r.containerClient.Pull(dockerService.Image, container.PullOptions{}); err != nil {
			return err
		}
		containers, err := r.containerClient.Create(
			dockerService.Image,
			container.CreateOptions{
				Input:         node.Input,
				Output:        node.Output,
				HasCommand:    len(node.Run) > 0,
				NumContainers: numContainers,
			},
		)
		if err != nil {
			return err
		}
		defer func() {
			for _, containerID := range containers {
				_ = r.containerClient.Kill(containerID, container.KillOptions{})
				_ = r.containerClient.Remove(containerID, container.RemoveOptions{})
			}
		}()
		for _, containerID := range containers {
			if err := r.containerClient.Start(
				containerID,
				container.StartOptions{
					Commands: node.Run,
				},
			); err != nil {
				return err
			}
		}
		for _, containerID := range containers {
			if err := r.containerClient.Wait(containerID, container.WaitOptions{}); err != nil {
				return err
			}
		}
		return nil
	}, nil
}

type dummyNodeErrorRecorder struct{}

func (d *dummyNodeErrorRecorder) Record(nodeName string, err error) {
	log.Printf("%s had error %v\n", nodeName, err)
}
