package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/google/go-github/v32/github"
)

// policy enforces the main build policy:
// - only build Draft PRs if the configured to do so.
// - only build PRs that are targeted at one of our valid base branches
func (ws *workflowSyncer) policy(
	ctx context.Context,
	updater ghCheckRunUpdater,
	repo *github.Repository,
	headBranch string,
	title string,
	prs []*github.PullRequest,
	crID int64,
) bool {

	if len(prs) > 0 {
		for _, pr := range prs {
			// TODO(tcolgate): I think this is never actually the case for web events. External PRs aren't
			// included
			if *pr.Head.Repo.URL != *pr.Base.Repo.URL {
				ghUpdateCheckRun(
					ctx,
					updater,
					repo,
					crID,
					title,
					"refusing to build non-local PR, org members can run them manually using `/kube-ci run`",
					"completed",
					"failure",
				)
				log.Printf("not running %s %s, as it for a PR from a non-local branch", repo.GetFullName(), headBranch)
				return false
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
					updater,
					repo,
					crID,
					title,
					fmt.Sprintf("checks are not automatically run for base branches that do not match `%s`, you can run manually using `/kube-ci run`", ws.config.buildBranches.String()),
					"completed",
					"failure",
				)

				log.Printf("not running %s %s, base branch did not match %s", repo.GetFullName(), headBranch, ws.config.buildBranches.String())

				return false
			}
		}

		onlyDrafts := true
		for _, pr := range prs {
			onlyDrafts = onlyDrafts && pr.GetDraft()
		}

		if onlyDrafts && !ws.config.BuildDraftPRs {
			ghUpdateCheckRun(
				ctx,
				updater,
				repo,
				crID,
				title,
				"auto checks Draft PRs are disabled, you can run manually using `/kube-ci run`",
				"completed",
				"failure",
			)
			log.Printf("not running %s %s, as it is only used in draft PRs", repo.GetFullName(), headBranch)
			return false
		}

		return true
	}

	// this commit was not for a PR, so we confirm we should build for the head branch it was targettted at
	if ws.config.buildBranches != nil && !ws.config.buildBranches.MatchString(headBranch) {
		ghUpdateCheckRun(
			ctx,
			updater,
			repo,
			crID,
			title,
			fmt.Sprintf("checks are not automatically run for base branches that do not match `%s`, you can run manually using `/kube-ci run`", ws.config.buildBranches.String()),
			"completed",
			"failure",
		)
		log.Printf("not running %s %s, as it target unmatched base branches", repo.GetFullName(), headBranch)
		return false
	}

	return true
}

func (ws *workflowSyncer) runWorkflow(ctx context.Context, ghClient *github.Client, instID int64, repo *github.Repository, headsha, headbranch string, prs []*github.PullRequest) error {
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
		return nil
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
		return fmt.Errorf("creating check run failed, %w", err)
	}

	ghUpdateCheckRun(
		ctx,
		ghClient.Checks,
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
			ghClient.Checks,
			repo,
			*cr.ID,
			title,
			msg,
			"completed",
			"failure",
		)
		log.Printf("unable to parse workflow for %s (%s), %v", repo, headbranch, err)
		return nil
	}

	if !ws.policy(ctx, ghClient.Checks, repo, headbranch, title, prs, *cr.ID) {
		return nil
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
			ghClient.Checks,
			repo,
			*cr.ID,
			title,
			fmt.Sprintf("creation of cache volume failed, %v", err),
			"completed",
			"failure",
		)
		return err
	}

	_, err = ws.client.ArgoprojV1alpha1().Workflows(ws.config.Namespace).Create(wf)
	if err != nil {
		ghUpdateCheckRun(
			ctx,
			ghClient.Checks,
			repo,
			*cr.ID,
			title,
			fmt.Sprintf("argo workflow creation failed, %v", err),
			"completed",
			"failure",
		)

		return fmt.Errorf("workflow creation failed, %w", err)
	}

	return nil
}
