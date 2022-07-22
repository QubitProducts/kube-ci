package main

import (
	"context"
	"regexp"
	"strconv"
	"testing"

	workflow "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	"github.com/google/go-github/v45/github"
)

func (f *fixture) newWebhook() *hookHandler {
	return nil
}

func (f *fixture) runWebhook(ev interface{}, t *testing.T) {
	f.runWebhookHandler(ev, true, false, t)
}

//lint:ignore U1000 we will need this at some point
func (f *fixture) runWebookExpectError(ev interface{}, t *testing.T) {
	f.runWebhookHandler(ev, true, true, t)
}

func (f *fixture) runWebhookHandler(obj interface{}, startInformers bool, expectError bool, t *testing.T) {
	c, i, k8sI, gh := f.newController(f.config, t)
	if startInformers {
		stopCh := make(chan struct{})
		defer close(stopCh)
		i.Start(stopCh)
		k8sI.Start(stopCh)
	}

	var err error
	var wf *workflow.Workflow
	switch obj := obj.(type) {
	case *workflow.Workflow:
		wf, err = c.sync(obj)
		if !expectError && err != nil {
			f.t.Errorf("error syncing workflow: %v", err)
		} else if expectError && err == nil {
			f.t.Error("expected error syncing workflow, got nil")
		}
	default:
	}

	compareActions("workflow actions", f.wfActions, f.wfClient.Actions(), t)
	compareActions("kubernetes actions", f.k8sActions, f.k8sClient.Actions(), t)

	crid, err := strconv.Atoi(wf.Annotations[annCheckRunID])
	if err != nil {
		f.t.Errorf("final workflow has invalid check-run id, %v", err)
	}
	status, actions := gh.getCheckRunStatus(int64(crid))
	compare("wrong Github Check Run status", f.githubCheckRunStatus, status, f.t)
	compare("wrong check-run action buttons", f.githubCheckRunActions, actions, f.t)

	compareGithubActions("wrong github calls", f.githubCalls, gh.calls, f.t)
}

type workflowRunnerFake struct {
}

func (w *workflowRunnerFake) runWorkflow(ctx context.Context, ghClient wfGHClient, repo *github.Repository, headsha, headreftype, headbranch, entrypoint string, prs []*github.PullRequest, deployevent *github.DeploymentEvent) error {
	panic("not implemented")
}

/*
type hookHandler struct {
	storage pvcManager
	clients githubClientSource
	runner  workflowRunner
	slash   slashRunner

	uiBase   string
	ghSecret []byte
	appID    int64
}
*/

func TestWebhookHandler_webhookPayload(t *testing.T) {
	var config Config
	config.deployTemplates = regexp.MustCompile("^$")
	config.actionTemplates = regexp.MustCompile("^$")
	config.productionEnvironments = regexp.MustCompile("^$")

	var tests = []struct {
		name         string
		setup        []setupf
		expectStatus github.CheckRun
	}{}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.config = config

			for _, setup := range tt.setup {
				setup(f)
			}

			f.wfObjects = append(f.wfObjects, f.wf)
			f.githubCheckRunStatus = tt.expectStatus

			f.runWebhook(f.wf, t)
		})
	}
}
