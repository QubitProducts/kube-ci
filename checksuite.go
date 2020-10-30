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
	"os"
	"time"

	"github.com/google/go-github/v32/github"
)

var (
	checkRunName          = "Argo Workflow"
	initialCheckRunStatus = github.String("queued")
)

// policy enforces the main build policy:
// - only build Draft PRs if the configured to do so.
// - only build PRs that are targeted at one of our valid base branches
func (ws *workflowSyncer) policy(
	ctx context.Context,
	ghClient *github.Client,
	repo *github.Repository,
	headBranch string,
	title string,
	prs []*github.PullRequest,
	cr *github.CheckRun,
) (int, string) {

	if len(prs) > 0 {
		for _, pr := range prs {
			if *pr.Head.Repo.URL != *pr.Base.Repo.URL ||
				*pr.Head.Repo.URL != *repo.URL {
				ghUpdateCheckRun(
					ctx,
					ghClient,
					repo,
					*cr.ID,
					title,
					"refusing to build non-local PR, org members can run them manually using `/kube-ci run`",
					"completed",
					"failure",
				)
				log.Printf("not running %s %s, as it for a PR from a non-local branch", repo.GetFullName(), headBranch)
				return http.StatusOK, "not building non local PR"
			}
		}

		if ws.config.buildBranches != nil {
			baseMatched := false
			for _, pr := range prs {
				if ws.config.buildBranches.MatchString(pr.GetBase().GetRef()) {
					baseMatched = true
					break
				}
			}

			if !baseMatched {
				ghUpdateCheckRun(
					ctx,
					ghClient,
					repo,
					*cr.ID,
					title,
					fmt.Sprintf("checks are not automatically run for base branches that do not match `%s`, you can run manually using `/kube-ci run`", ws.config.buildBranches.String()),
					"completed",
					"failure",
				)

				log.Printf("not running %s %s, base branch did not match %s", repo.GetFullName(), headBranch, ws.config.buildBranches.String())

				return http.StatusOK, "skipping unmatched base branch"
			}
		}

		onlyDrafts := true
		for _, pr := range prs {
			onlyDrafts = onlyDrafts && pr.GetDraft()
		}

		if onlyDrafts && !ws.config.BuildDraftPRs {
			ghUpdateCheckRun(
				ctx,
				ghClient,
				repo,
				*cr.ID,
				title,
				"auto checks Draft PRs are disabled, you can run manually using `/kube-ci run`",
				"completed",
				"failure",
			)
			log.Printf("not running %s %s, as it is only used in draft PRs", repo.GetFullName(), headBranch)
			return http.StatusOK, "skipping draft PR"
		}

		return 0, ""
	}

	// this commit was not for a PR, so we confirm we should build for the head branch it was targettted at
	if ws.config.buildBranches != nil && !ws.config.buildBranches.MatchString(headBranch) {
		ghUpdateCheckRun(
			ctx,
			ghClient,
			repo,
			*cr.ID,
			title,
			fmt.Sprintf("checks are not automatically run for base branches that do not match `%s`, you can run manually using `/kube-ci run`", ws.config.buildBranches.String()),
			"completed",
			"failure",
		)
		log.Printf("not running %s %s, as it target unmatched base branches", repo.GetFullName(), headBranch)
		return http.StatusOK, "skipping PR to unmatched base branch "
	}

	return 0, ""
}

func ghUpdateCheckRun(
	ctx context.Context,
	ghClient *github.Client,
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
	_, _, err := ghClient.Checks.UpdateCheckRun(
		ctx,
		org,
		repoName,
		crID,
		opts)

	if err != nil {
		log.Printf("Update of aborted check run failed, %v", err)
	}
}

type orgClient struct {
}

func (ws *workflowSyncer) webhookCreateTag(ctx context.Context, event *github.CreateEvent) (int, string) {
	return 0, ""
}

func (ws *workflowSyncer) runWorkflow(ctx context.Context, ghClient *github.Client, instID int64, repo *github.Repository, headsha, headbranch string, prs []*github.PullRequest) (int, string) {
	org := repo.GetOwner().GetLogin()
	name := repo.GetName()
	wf, err := ws.getWorkflow(
		ctx,
		ghClient,
		repo,
		headsha,
		ws.config.CIFilePath,
	)

	if os.IsNotExist(err) {
		log.Printf("no %s in %s/%s (%s)",
			ws.config.CIFilePath,
			org,
			name,
			headsha,
		)
		return http.StatusOK, ""
	}

	title := "Workflow Setup"
	cr, _, crerr := ghClient.Checks.CreateCheckRun(ctx,
		org,
		name,
		github.CreateCheckRunOptions{
			Name:    checkRunName,
			HeadSHA: headsha,
			Status:  initialCheckRunStatus,
			Output: &github.CheckRunOutput{
				Title:   &title,
				Summary: github.String("Creating workflow"),
			},
		},
	)
	if crerr != nil {
		log.Printf("Unable to create check run, %v", err)
		return http.StatusInternalServerError, ""
	}

	ghUpdateCheckRun(
		ctx,
		ghClient,
		repo,
		*cr.ID,
		title,
		"Creating Workflow",
		"queued",
		"",
	)

	if err != nil {
		msg := fmt.Sprintf("unable to parse workflow, %v", err)
		ghUpdateCheckRun(
			ctx,
			ghClient,
			repo,
			*cr.ID,
			title,
			msg,
			"completed",
			"failure",
		)
		return http.StatusOK, msg
	}

	if status, msg := ws.policy(ctx, ghClient, repo, headbranch, title, prs, cr); status != 0 {
		return status, msg
	}

	ws.cancelRunningWorkflows(
		labelSafe(org),
		labelSafe(name),
		labelSafe(headbranch),
	)

	wf = wf.DeepCopy()
	ws.updateWorkflow(
		wf,
		instID,
		repo,
		prs,
		headsha,
		headbranch,
		cr,
	)

	err = ws.ensurePVC(
		wf,
		org,
		name,
		headbranch,
		ws.config.CacheDefaults,
	)
	if err != nil {
		ghUpdateCheckRun(
			ctx,
			ghClient,
			repo,
			*cr.ID,
			title,
			fmt.Sprintf("creation of cache volume failed, %v", err),
			"completed",
			"failure",
		)
	}

	_, err = ws.client.ArgoprojV1alpha1().Workflows(ws.config.Namespace).Create(wf)
	if err != nil {
		ghUpdateCheckRun(
			ctx,
			ghClient,
			repo,
			*cr.ID,
			title,
			fmt.Sprintf("argo workflow creation failed, %v", err),
			"completed",
			"failure",
		)

		return http.StatusInternalServerError, ""
	}

	return http.StatusOK, ""
}

func (ws *workflowSyncer) webhookCheckSuite(ctx context.Context, event *github.CheckSuiteEvent) (int, string) {
	ghClient, err := ws.ghClientSrc.getClient(*event.Org.Login, int(*event.Installation.ID))
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}
	return ws.runWorkflow(
		ctx,
		ghClient,
		event.Installation.GetID(),
		event.Repo,
		*event.CheckSuite.HeadSHA,
		*event.CheckSuite.HeadBranch,
		event.CheckSuite.PullRequests,
	)

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
