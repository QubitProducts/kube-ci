package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v32/github"
)

type ghClientInterface interface {
	GetInstallID() int
	GetRef(ctx context.Context, ref string) (*github.Reference, error)
	UpdateCheckRun(ctx context.Context, id int64, upd github.UpdateCheckRunOptions) (*github.CheckRun, error)
	StatusUpdate(
		ctx context.Context,
		crID int64,
		title string,
		msg string,
		status string,
		conclusion string,
	)
	CreateCheckRun(ctx context.Context, opts github.CreateCheckRunOptions) (*github.CheckRun, error)
	CreateDeployment(ctx context.Context, req *github.DeploymentRequest) (*github.Deployment, error)
	CreateDeploymentStatus(ctx context.Context, id int64, req *github.DeploymentStatusRequest) (*github.DeploymentStatus, error)
	IsMember(ctx context.Context, user string) (bool, error)
	DownloadContents(ctx context.Context, filepath string, opts *github.RepositoryContentGetOptions) (io.ReadCloser, error)
	CreateFile(ctx context.Context, filepath string, opts *github.RepositoryContentFileOptions) error
	GetContents(ctx context.Context, filepath string, opts *github.RepositoryContentGetOptions) ([]*github.RepositoryContent, error)
	GetBranch(ctx context.Context, branch string) (*github.Branch, error)
	GetPullRequest(ctx context.Context, prid int) (*github.PullRequest, error)
	CreateIssueComment(ctx context.Context, issueID int, opts *github.IssueComment) error
}

type githubKeyStore struct {
	baseTransport http.RoundTripper
	appID         int64
	ids           []int
	key           []byte
	orgs          *regexp.Regexp
}

func (ks *githubKeyStore) getClient(org string, installID int, repo string) (ghClientInterface, error) {
	validID := false
	for _, id := range ks.ids {
		if installID == id {
			validID = true
			break
		}
	}

	if len(ks.ids) > 0 && !validID {
		return nil, fmt.Errorf("unknown installation %d", installID)
	}

	if ks.orgs != nil && !ks.orgs.MatchString(org) {
		return nil, fmt.Errorf("refusing event from untrusted org %s", org)
	}

	itr, err := ghinstallation.New(ks.baseTransport, ks.appID, int64(installID), ks.key)
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
	getClient(org string, installID int, repo string) (ghClientInterface, error)
}
