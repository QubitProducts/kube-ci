package main

import (
	"context"
	"log"
	"regexp"
	"testing"
	"time"

	"github.com/google/go-github/v32/github"
)

//lint:ignore U1000 this will be used again
type checkRunUpdateRecorder struct {
	org     string
	repo    string
	updates []github.UpdateCheckRunOptions

	now func() time.Time
}

func (crr *checkRunUpdateRecorder) UpdateCheckRun(ctx context.Context, owner, repo string, checkRunID int64, opts github.UpdateCheckRunOptions) (*github.CheckRun, *github.Response, error) {
	crr.updates = append(crr.updates, opts)
	return nil, nil, nil
}

func (crr *checkRunUpdateRecorder) StatusUpdate(
	ctx context.Context,
	info *githubInfo,
	status GithubStatus,
) {
	log.Print(status.summary)
	opts := github.UpdateCheckRunOptions{
		Name:   defaultCheckRunName,
		Status: &status.status,
		Output: &github.CheckRunOutput{
			Title:   &status.title,
			Summary: &status.summary,
		},
	}

	opts.Actions = status.Actions

	if status.conclusion != "" {
		opts.Conclusion = &status.conclusion
		opts.CompletedAt = &github.Timestamp{
			Time: crr.now(),
		}
	}
	_, _, err := crr.UpdateCheckRun(
		ctx,
		crr.org,
		crr.repo,
		info.checkRunID,
		opts)

	if err != nil {
		log.Printf("Update of aborted check run failed, %v", err)
	}
}

func TestPolicy(t *testing.T) {
	for _, st := range []struct {
		testName string

		config     Config
		repo       *github.Repository
		headbranch string
		title      string
		prs        []*github.PullRequest

		exp *policyRejection
	}{
		{
			testName:   "default behaviour (non-PR)",
			config:     Config{},
			repo:       &github.Repository{},
			headbranch: "master",
			prs:        nil,

			exp: nil,
		},
		{
			testName: "unmatched build branch",

			config: Config{
				buildBranches: regexp.MustCompile("^main$"),
			},
			repo:       &github.Repository{},
			headbranch: "master",
			prs:        nil,

			exp: &policyRejection{message: "checks are not automatically run for base branches that do not match `^main$`, you can run manually using `/kube-ci run`"},
		},
		{
			testName: "one non-draft PR",

			repo:       &github.Repository{},
			headbranch: "master",
			prs: []*github.PullRequest{
				{
					Head:  &github.PullRequestBranch{Repo: &github.Repository{URL: github.String("http://localhost/blah")}},
					Base:  &github.PullRequestBranch{Repo: &github.Repository{URL: github.String("http://localhost/blah")}},
					Draft: github.Bool(false),
				},
			},

			exp: nil,
		}, {
			testName: "one non-draft PR, unmatched branch",

			config: Config{
				buildBranches: regexp.MustCompile("^main$"),
			},
			repo:       &github.Repository{},
			headbranch: "master",
			prs: []*github.PullRequest{
				{
					Head:  &github.PullRequestBranch{Repo: &github.Repository{URL: github.String("http://localhost/blah")}},
					Base:  &github.PullRequestBranch{Repo: &github.Repository{URL: github.String("http://localhost/blah")}},
					Draft: github.Bool(false),
				},
			},

			exp: &policyRejection{message: "checks are not automatically run for base branches that do not match `^main$`, you can run manually using `/kube-ci run`"},
		},
		{
			testName: "one draft PR",

			config:     Config{BuildDraftPRs: true},
			repo:       &github.Repository{},
			headbranch: "master",
			prs: []*github.PullRequest{
				{
					Head:  &github.PullRequestBranch{Repo: &github.Repository{URL: github.String("http://localhost/blah")}},
					Base:  &github.PullRequestBranch{Repo: &github.Repository{URL: github.String("http://localhost/blah")}},
					Draft: github.Bool(true),
				},
			},

			exp: nil,
		},
		{
			testName: "one draft PR",

			config:     Config{BuildDraftPRs: false},
			repo:       &github.Repository{},
			headbranch: "master",
			prs: []*github.PullRequest{
				{
					Head:  &github.PullRequestBranch{Repo: &github.Repository{URL: github.String("http://localhost/blah")}},
					Base:  &github.PullRequestBranch{Repo: &github.Repository{URL: github.String("http://localhost/blah")}},
					Draft: github.Bool(true),
				},
			},

			exp: &policyRejection{message: "auto checks Draft PRs are disabled, you can run manually using `/kube-ci run`"},
		},
	} {
		st := st
		ws := workflowSyncer{config: st.config}

		t.Run(st.testName, func(t *testing.T) {
			err := ws.policy(
				st.repo,
				st.headbranch,
				st.title,
				st.prs,
			)
			if err != nil {
				if st.exp == nil {
					t.Fatalf("unexpected policy rejection, got %v", err)
					return
				}
				err.log = ""
				if *err != *st.exp {
					t.Fatalf("expected policy to evaluate to %v, got %v", st.exp, err)
				}
				return
			}

			if st.exp != nil {
				t.Fatalf("did not get expected policy rejection %v", st.exp)
			}

		})
	}
}
