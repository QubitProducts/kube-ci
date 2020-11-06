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
	"time"

	"github.com/google/go-github/v32/github"
)

var (
	checkRunName          = "Argo Workflow"
	initialCheckRunStatus = github.String("queued")
)

type ghCheckRunUpdater interface {
	UpdateCheckRun(
		ctx context.Context,
		org string,
		repoName string,
		crID int64,
		opts github.UpdateCheckRunOptions,
	) (*github.CheckRun, *github.Response, error)
}

func ghUpdateCheckRun(
	ctx context.Context,
	updater ghCheckRunUpdater,
	repo *github.Repository,
	crID int64,
	title string,
	msg string,
	status string,
	conclusion string,
) {
	org := repo.GetOwner().GetLogin()
	repoName := repo.GetName()

	log.Print(msg)
	opts := github.UpdateCheckRunOptions{
		Name:   checkRunName,
		Status: &status,
		Output: &github.CheckRunOutput{
			Title:   &title,
			Summary: &msg,
		},
	}

	if conclusion != "" {
		opts.Conclusion = &conclusion
		opts.CompletedAt = &github.Timestamp{
			Time: time.Now(),
		}
	}
	_, _, err := updater.UpdateCheckRun(
		ctx,
		org,
		repoName,
		crID,
		opts)

	if err != nil {
		log.Printf("Update of aborted check run failed, %v", err)
	}
}

func (ws *workflowSyncer) webhookCreateTag(ctx context.Context, event *github.CreateEvent) (int, string) {
	owner := event.Repo.Owner.GetLogin()
	ghClient, err := ws.ghClientSrc.getClient(owner, int(*event.Installation.ID))
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	ref, _, err := ghClient.Git.GetRef(
		ctx,
		owner,
		event.Repo.GetName(),
		"tags/"+event.GetRef(),
	)

	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	headSHA := ref.Object.GetSHA()

	err = ws.runWorkflow(
		ctx,
		ghClient,
		event.Installation.GetID(),
		event.Repo,
		headSHA,
		"tag",
		event.GetRef(),
		nil,
	)

	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	return http.StatusOK, ""
}

func (ws *workflowSyncer) webhookCheckSuite(ctx context.Context, event *github.CheckSuiteEvent) (int, string) {
	ghClient, err := ws.ghClientSrc.getClient(*event.Org.Login, int(*event.Installation.ID))
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	err = ws.runWorkflow(
		ctx,
		ghClient,
		event.Installation.GetID(),
		event.Repo,
		*event.CheckSuite.HeadSHA,
		"branch",
		*event.CheckSuite.HeadBranch,
		event.CheckSuite.PullRequests,
	)

	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	return http.StatusOK, ""
}

func (ws *workflowSyncer) webhookCheckRunRequestAction(ctx context.Context, event *github.CheckRunEvent) (int, string) {
	ghClient, err := ws.ghClientSrc.getClient(*event.Org.Login, int(*event.Installation.ID))
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
	dep, _, err := ghClient.Repositories.CreateDeployment(
		ctx,
		*event.Org.Login,
		*event.Repo.Name,
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
