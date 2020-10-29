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
	"crypto/sha1"
	"encoding/base32"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/go-github/v32/github"
	"k8s.io/apimachinery/pkg/labels"
)

var (
	checkRunName          = "Argo Workflow"
	initialCheckRunStatus = github.String("queued")
)

func detailsHash(org, repo, branch string) string {
	h := sha1.New()
	_, _ = h.Write([]byte(org))
	_, _ = h.Write([]byte(repo))
	_, _ = h.Write([]byte(branch))
	bs := h.Sum(nil)
	str := base32.StdEncoding.EncodeToString(bs)
	return "ci" + strings.Replace(str, "=+/", "", -1)
}

// policy enforces the main build policy:
// - only build Draft PRs if the configured to do so.
// - only build PRs that are targeted at one of our valid base branches
func (ws *workflowSyncer) policy(ctx context.Context, ghClient *github.Client, event *github.CheckSuiteEvent, title string, cr *github.CheckRun) (int, string) {
	if len(event.CheckSuite.PullRequests) > 0 {
		for _, pr := range event.CheckSuite.PullRequests {
			if *pr.Head.Repo.URL != *pr.Base.Repo.URL ||
				*pr.Head.Repo.URL != *event.Repo.URL {
				ghUpdateCheckRun(
					ctx,
					ghClient,
					*event.Org.Login,
					*event.Repo.Name,
					*cr.ID,
					title,
					fmt.Sprintf("refusing to build non-local PR, org members can run them manually using `/kube-ci run`"),
					"completed",
					"failure",
				)
				log.Printf("not running %s %s, as it for a PR from a non-local branch", event.Repo.GetFullName(), event.CheckSuite.GetHeadBranch())
				return http.StatusOK, "not building non local PR"
			}
		}

		if ws.config.buildBranches != nil {
			baseMatched := false
			for _, pr := range event.CheckSuite.PullRequests {
				if ws.config.buildBranches.MatchString(pr.GetBase().GetRef()) {
					baseMatched = true
					break
				}
			}

			if !baseMatched {
				ghUpdateCheckRun(
					ctx,
					ghClient,
					*event.Org.Login,
					*event.Repo.Name,
					*cr.ID,
					title,
					fmt.Sprintf("checks are not automatically run for base branches that do not match `%s`, you can run manually using `/kube-ci run`", ws.config.buildBranches.String()),
					"completed",
					"failure",
				)

				log.Printf("not running %s %s, base branch did not match %s", event.Repo.GetFullName(), event.CheckSuite.GetHeadBranch(), ws.config.buildBranches.String())

				return http.StatusOK, "skipping unmatched base branch"
			}
		}

		onlyDrafts := true
		for _, pr := range event.CheckSuite.PullRequests {
			onlyDrafts = onlyDrafts && pr.GetDraft()
		}

		if onlyDrafts && !ws.config.BuildDraftPRs {
			ghUpdateCheckRun(
				ctx,
				ghClient,
				*event.Org.Login,
				*event.Repo.Name,
				*cr.ID,
				title,
				fmt.Sprintf("auto checks Draft PRs are disabled, you can run manually using `/kube-ci run`"),
				"completed",
				"failure",
			)
			log.Printf("not running %s %s, as it is only used in draft PRs", event.Repo.GetFullName(), event.CheckSuite.GetHeadBranch())
			return http.StatusOK, "skipping draft PR"
		}

		return 0, ""
	}

	// this commit was not for a PR, so we confirm we should build for the head branch it was targettted at
	if ws.config.buildBranches != nil && !ws.config.buildBranches.MatchString(event.CheckSuite.GetHeadBranch()) {
		ghUpdateCheckRun(
			ctx,
			ghClient,
			*event.Org.Login,
			*event.Repo.Name,
			*cr.ID,
			title,
			fmt.Sprintf("checks are not automatically run for base branches that do not match `%s`, you can run manually using `/kube-ci run`", ws.config.buildBranches.String()),
			"completed",
			"failure",
		)
		log.Printf("not running %s %s, as it target unmatched base branches", event.Repo.GetFullName(), event.CheckSuite.GetHeadBranch())
		return http.StatusOK, "skipping PR to unmatched base branch "
	}

	return 0, ""
}

func (ws *workflowSyncer) webhookCheckSuite(ctx context.Context, event *github.CheckSuiteEvent) (int, string) {
	ghClient, err := ws.ghClientSrc.getClient(*event.Org.Login, int(*event.Installation.ID))
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	wf, err := ws.getWorkflow(
		ctx,
		ghClient,
		*event.Org.Login,
		*event.Repo.Name,
		*event.CheckSuite.HeadSHA,
		ws.config.CIFilePath,
	)

	if os.IsNotExist(err) {
		log.Printf("no %s in %s/%s (%s)",
			ws.config.CIFilePath,
			*event.Org.Login,
			*event.Repo.Name,
			*event.CheckSuite.HeadSHA,
		)
		return http.StatusOK, ""
	}

	title := "Workflow Setup"
	cr, _, crerr := ghClient.Checks.CreateCheckRun(ctx,
		*event.Org.Login,
		*event.Repo.Name,
		github.CreateCheckRunOptions{
			Name:    checkRunName,
			HeadSHA: *event.CheckSuite.HeadSHA,
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
		*event.Org.Login,
		*event.Repo.Name,
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
			*event.Org.Login,
			*event.Repo.Name,
			*cr.ID,
			title,
			msg,
			"completed",
			"failure",
		)
		return http.StatusOK, msg
	}

	if status, msg := ws.policy(ctx, ghClient, event, title, cr); status != 0 {
		return status, msg
	}

	// We'll cancel all in-progress checks for this
	// repo/branch
	wfs, err := ws.lister.Workflows(ws.config.Namespace).List(labels.Set(
		map[string]string{
			labelOrg:         labelSafe(*event.Repo.Owner.Login),
			labelRepo:        labelSafe(*event.Repo.Name),
			labelBranch:      labelSafe(*event.CheckSuite.HeadBranch),
			labelDetailsHash: detailsHash(*event.Repo.Owner.Login, *event.Repo.Name, *event.CheckSuite.HeadBranch),
		}).AsSelector())

	for _, wf := range wfs {
		wf = wf.DeepCopy()
		ads := int64(0)
		wf.Spec.ActiveDeadlineSeconds = &ads
		ws.client.ArgoprojV1alpha1().Workflows(wf.Namespace).Update(wf)
	}

	wf = wf.DeepCopy()
	ws.updateWorkflow(wf, event, cr)

	err = ws.ensurePVC(
		wf,
		*event.Org.Login,
		*event.Repo.Name,
		*event.CheckSuite.HeadBranch,
		ws.config.CacheDefaults,
	)
	if err != nil {
		ghUpdateCheckRun(
			ctx,
			ghClient,
			*event.Org.Login,
			*event.Repo.Name,
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
			*event.Org.Login,
			*event.Repo.Name,
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

func ghUpdateCheckRun(
	ctx context.Context,
	ghClient *github.Client,
	org string,
	repo string,
	crID int64,
	title string,
	msg string,
	status string,
	conclusion string,
) {
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
		repo,
		crID,
		opts)

	if err != nil {
		log.Printf("Update of aborted check run failed, %v", err)
	}
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
