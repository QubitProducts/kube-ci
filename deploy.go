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

	"github.com/google/go-github/v22/github"
)

func (ws *workflowSyncer) webhookDeployment(ctx context.Context, event *github.DeploymentEvent) (int, string) {
	org := event.GetRepo().GetOwner().Login

	ghClient, err := ws.ghClientSrc.getClient(*org, int(*event.Installation.ID))
	if err != nil {
		return http.StatusBadRequest, "failed to create github client"
	}

	user := event.Deployment.Creator.Name
	ok, _, err := ghClient.Organizations.IsMember(ctx, *org, *user)
	if err != nil {
		return http.StatusBadRequest, "failed to check org membership"
	}

	if !ok {
		return http.StatusBadRequest, "deployment user not from our orgs"
	}

	logURL := fmt.Sprintf(
		"%s/workflows/%s/%s",
		ws.argoUIBase,
		"blah",
		"blah")

	pending := "pending"
	_, _, err = ghClient.Repositories.CreateDeploymentStatus(
		ctx,
		*event.Repo.Owner.Login,
		*event.Repo.Name,
		*event.Deployment.ID,
		&github.DeploymentStatusRequest{
			State:  &pending,
			LogURL: &logURL,
		},
	)

	if err != nil {
		log.Printf("create deployment state failed, %v", err)
		return http.StatusInternalServerError, ""
	}

	go func() {
		time.Sleep(10 * time.Second)
		success := "success"
		_, _, err := ghClient.Repositories.CreateDeploymentStatus(
			context.Background(),
			*event.Repo.Owner.Login,
			*event.Repo.Name,
			*event.Deployment.ID,
			&github.DeploymentStatusRequest{
				State:  &success,
				LogURL: &logURL,
			},
		)

		if err != nil {
			log.Printf("create deployment state failed, %v", err)
		}
	}()

	return http.StatusOK, ""
}

func (ws *workflowSyncer) webhookDeploymentStatus(ctx context.Context, event *github.DeploymentStatusEvent) (int, string) {
	log.Printf("status: %v is %v", *event.DeploymentStatus.ID, *event.DeploymentStatus.State)
	return http.StatusOK, ""
}
