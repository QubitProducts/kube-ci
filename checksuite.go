// Copyright 2019 Qubit Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/google/go-github/v32/github"
)

var (
	checkRunName          = "Argo Workflow"
	initialCheckRunStatus = github.String("queued")
)

type ghCheckRunUpdater interface {
	UpdateCheckRun(
		ctx context.Context,
		crID int64,
		opts github.UpdateCheckRunOptions,
	) (*github.CheckRun, error)
}

func (ws *workflowSyncer) webhookCreateTag(ctx context.Context, event *github.CreateEvent) (int, string) {
	owner := event.Repo.Owner.GetLogin()
	repo := event.Repo.GetName()
	ghClient, err := ws.ghClientSrc.getClient(owner, int(*event.Installation.ID), repo, owner)
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	ref, err := ghClient.GetRef(
		ctx,
		"tags/"+event.GetRef(),
	)

	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	headSHA := ref.Object.GetSHA()

	err = ws.runWorkflow(
		ctx,
		ghClient,
		event.Repo,
		headSHA,
		"tag",
		event.GetRef(),
		nil,
		ghClient,
	)

	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	return http.StatusOK, ""
}

func (ws *workflowSyncer) webhookCheckSuite(ctx context.Context, event *github.CheckSuiteEvent) (int, string) {
	org := *event.Org.Login
	repo := event.Repo.GetName()
	ghClient, err := ws.ghClientSrc.getClient(*event.Org.Login, int(*event.Installation.ID), org, repo)
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	err = ws.runWorkflow(
		ctx,
		ghClient,
		event.Repo,
		*event.CheckSuite.HeadSHA,
		"branch",
		*event.CheckSuite.HeadBranch,
		event.CheckSuite.PullRequests,
		ghClient,
	)

	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	return http.StatusOK, ""
}

func (ws *workflowSyncer) webhookCheckRunRequestAction(ctx context.Context, event *github.CheckRunEvent) (int, string) {
	repo := *event.Repo.Name
	owner := event.Repo.Owner.GetName()
	ghClient, err := ws.ghClientSrc.getClient(*event.Org.Login, int(*event.Installation.ID), repo, owner)
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	/*
			// All is good (return an error to fail)
			ciFile := ".kube-ci/deploy.yaml"

			wf, err := ws.getWorkflow(
				ctx,
				ghClient,
				*event.Org.Login,
				*event.Repo.Name,
				*event.CheckRun.HeadSHA,
				ciFile,
			)

		if os.IsNotExist(err) {
			log.Printf("no %s in %s/%s (%s)",
				ciFile,
				*event.Org.Login,
				*event.Repo.Name,
				*event.CheckRun.HeadSHA,
			)
			return http.StatusOK, ""
		}
	*/

	env := "staging"
	msg := fmt.Sprintf("deploying the thing to %v", env)
	dep, err := ghClient.CreateDeployment(
		ctx,
		&github.DeploymentRequest{
			Ref:         event.CheckRun.HeadSHA,
			Description: &msg,
			Environment: &env,
		},
	)

	if err != nil {
		log.Printf("create deployment ailed, %v", err)
		return http.StatusInternalServerError, ""
	}

	log.Printf("Deployment created, %v", *dep.ID)

	return http.StatusOK, "blah"
}
