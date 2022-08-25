package kubeci

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-github/v45/github"
)

var rfc1035Re = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

func TestWFName(t *testing.T) {
	var tests = []struct {
		org        string
		repo       string
		entrypoint string
		ref        string

		expected string
	}{
		{"QubitProducts", "kube-ci", "", "master", "kube-ci.master"},
		{"QubitProducts", "kube-ci", "deploy", "master", "kube-ci.deploy.master"},
		{"QubitProducts", "kube-ci", "deploy:migrations", "master", "kube-ci.deploy-migratio.master"},
		{"myorg", "myrepo", "", "user-project-1234-some-really-long-branch-namme-from-some-dirt-bag-issue-tracking-system", "myrepo.user-project-1234-some-really-lon"},
		{"QubitProdcts", "quite-long-repo-name", "", "user-project-1234-some-really-long-branch-namme-from-some-dirt-bag-issue-tracking-system", "quite-long-repo-name.user-project-1234-s"},
		{"QubitProdcts", "quite-long-repo-name", "deploy:migrations", "user-project-1234-some-really-long-branch-namme-from-some-dirt-bag-issue-tracking-system", "quite-long-repo.deploy-migratio.user-pro"},
	}
	for i, tt := range tests {
		tt := tt
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			actual := wfName(tt.org, tt.repo, tt.entrypoint, tt.ref)
			t.Logf("name: %v", actual)

			if len(actual) > 50 {
				t.Errorf("workflow name %s is too long (should be <= 50)", actual)
			}

			parts := strings.Split(actual, ".")

			for _, p := range parts {
				if !rfc1035Re.MatchString(p) {
					t.Errorf("%s in %s to match rfc1035", p, actual)
				}
			}

			parts = parts[0 : len(parts)-1]

			hash := parts[len(parts)-1]
			if hash == "" {
				t.Errorf("workflow name %s did not include a hash", actual)
			}

			actual = strings.Join(parts, ".")
			if actual != tt.expected {
				t.Errorf("expected %s, actual %s", tt.expected, actual)
			}
		})
	}
}

func FuzzWFName(f *testing.F) {
	//
	f.Add("myorg", "myrepo", "", "master")
	f.Add("myorg", "myrepo", "deploy", "master")
	f.Fuzz(func(t *testing.T, org, repo, entrypoint, ref string) {
		actual := wfName(org, repo, entrypoint, ref)

		if len(actual) > 50 {
			t.Errorf("workflow name %s is too long (should be <= 50)", actual)
		}

		parts := strings.Split(actual, ".")
		for _, p := range parts {
			if !rfc1035Re.MatchString(p) {
				t.Errorf("%s in %s to match rfc1035", p, actual)
			}
		}
	})
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
