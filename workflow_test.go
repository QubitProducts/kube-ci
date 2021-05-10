package main

import (
	"context"
	"log"
	"regexp"
	"testing"
	"time"

	"github.com/google/go-github/v32/github"
)

type checkRunUpdateRecorder struct {
	t       *testing.T
	org     string
	repo    string
	updates []github.UpdateCheckRunOptions
}

func (crr *checkRunUpdateRecorder) UpdateCheckRun(ctx context.Context, owner, repo string, checkRunID int64, opts github.UpdateCheckRunOptions) (*github.CheckRun, *github.Response, error) {
	crr.updates = append(crr.updates, opts)
	return nil, nil, nil
}

func (crr *checkRunUpdateRecorder) StatusUpdate(
	ctx context.Context,
	opts StatusUpdateOpts,
) {
	crr.t.Logf("%#v", opts)
	uopts := github.UpdateCheckRunOptions{
		Name:   checkRunName,
		Status: &opts.status,
		Output: &github.CheckRunOutput{
			Title:   &opts.title,
			Summary: &opts.summary,
		},
	}

	if opts.conclusion != "" {
		uopts.Conclusion = &opts.conclusion
		uopts.CompletedAt = &github.Timestamp{
			Time: time.Now(),
		}
	}
	_, _, err := crr.UpdateCheckRun(
		ctx,
		crr.org,
		crr.repo,
		opts.crID,
		uopts)

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

		exp        bool
		expMessage string
	}{
		{
			testName:   "default behaviour (non-PR)",
			config:     Config{},
			repo:       &github.Repository{},
			headbranch: "master",
			prs:        nil,

			exp: true,
		},
		{
			testName: "unmatched build branch",

			config: Config{
				buildBranches: regexp.MustCompile("^main$"),
			},
			repo:       &github.Repository{},
			headbranch: "master",
			prs:        nil,

			exp:        false,
			expMessage: "checks are not automatically run for base branches that do not match `^main$`, you can run manually using `/kube-ci run`",
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

			exp: true,
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

			exp:        false,
			expMessage: "checks are not automatically run for base branches that do not match `^main$`, you can run manually using `/kube-ci run`",
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

			exp: true,
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

			exp:        false,
			expMessage: "auto checks Draft PRs are disabled, you can run manually using `/kube-ci run`",
		},
	} {
		st := st
		ws := workflowSyncer{config: st.config}
		crr := checkRunUpdateRecorder{t: t}

		t.Run(st.testName, func(t *testing.T) {
			ok := ws.policy(
				context.Background(),
				&crr,
				st.repo,
				st.headbranch,
				st.title,
				st.prs,
				1,
			)
			if ok != st.exp {
				t.Fatalf("expected policy to evaluate to %v, got %v", st.exp, ok)
			}
			if ok && len(crr.updates) != 0 {
				t.Fatalf("policy passed, but we got unexpected checkrun updates, %v", crr.updates)
				return
			}

			if ok {
				return
			}

			if len(crr.updates) == 0 {
				t.Fatalf("policy failed, but we got no explanatory check run update")
				return
			}
			if len(crr.updates) != 1 {
				t.Fatalf("only want one update, got %v", crr.updates)
				return
			}

			update := crr.updates[0]
			expConclusion := "failure"
			if update.GetConclusion() != expConclusion {
				t.Fatalf("expected checkrun conclusion %q, but got %q", expConclusion, update.GetConclusion())
			}
			expStatus := "completed"
			if update.GetStatus() != expStatus {
				t.Fatalf("expected checkrun status %q, but got %q", expStatus, update.GetStatus())
			}

			sum := update.GetOutput().GetSummary()
			if st.expMessage != sum {
				t.Fatalf("expected checkrun summary %q, but got %q", st.expMessage, sum)
			}
		})
	}
}
