package kubeci

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v45/github"
)

type GithubClientInterface interface {
	GetInstallID() int
	GetRef(ctx context.Context, ref string) (*github.Reference, error)
	CreateCheckRun(ctx context.Context, opts github.CreateCheckRunOptions) (*github.CheckRun, error)
	GetRepo(ctx context.Context) (*github.Repository, error)
	UpdateCheckRun(ctx context.Context, id int64, upd github.UpdateCheckRunOptions) (*github.CheckRun, error)
	CreateDeployment(ctx context.Context, req *github.DeploymentRequest) (*github.Deployment, error)
	CreateDeploymentStatus(ctx context.Context, id int64, req *github.DeploymentStatusRequest) (*github.DeploymentStatus, error)
	IsMember(ctx context.Context, user string) (bool, error)
	DownloadRepoContents(ctx context.Context, filepath string, opts *github.RepositoryContentGetOptions) (io.ReadCloser, error)
	DownloadContents(ctx context.Context, org, repo, filepath string, opts *github.RepositoryContentGetOptions) (io.ReadCloser, error)
	CreateFile(ctx context.Context, filepath string, opts *github.RepositoryContentFileOptions) error
	GetRepoContents(ctx context.Context, filepath string, opts *github.RepositoryContentGetOptions) ([]*github.RepositoryContent, error)
	GetBranch(ctx context.Context, branch string) (*github.Branch, error)
	GetPullRequest(ctx context.Context, prid int) (*github.PullRequest, error)
	CreateIssueComment(ctx context.Context, issueID int, opts *github.IssueComment) error
}

type GithubKeyStore struct {
	BaseTransport http.RoundTripper
	AppID         int64
	IDs           []int
	Key           []byte
	Orgs          *regexp.Regexp
}

func (ks *GithubKeyStore) getClient(org string, installID int, repo string) (GithubClientInterface, error) {
	validID := false
	for _, id := range ks.IDs {
		if installID == id {
			validID = true
			break
		}
	}

	if len(ks.IDs) > 0 && !validID {
		return nil, fmt.Errorf("unknown installation %d", installID)
	}

	if ks.Orgs != nil && !ks.Orgs.MatchString(org) {
		return nil, fmt.Errorf("refusing event from untrusted org %s", org)
	}

	itr, err := ghinstallation.New(ks.BaseTransport, ks.AppID, int64(installID), ks.Key)
	if err != nil {
		return nil, err
	}

	ghc, err := github.NewClient(&http.Client{Transport: itr}), nil
	if err != nil {
		return nil, err
	}

	return &repoClient{
		installID: installID,
		org:       org,
		client:    ghc,
		repo:      repo,
	}, nil
}

type githubClientSource interface {
	getClient(org string, installID int, repo string) (GithubClientInterface, error)
}
