package main

import (
	"context"
	"io"
	"testing"

	"github.com/google/go-github/v32/github"
)

//lint:ignore U1000 this will be used once we need to mock the GH client
type testGHClientInterface struct {
	instID int
	t      *testing.T
}

func (tgi *testGHClientInterface) GetInstallID() int {
	return tgi.instID
}

func (tgi *testGHClientInterface) GetRef(ctx context.Context, ref string) (*github.Reference, error) {
	panic("not implemented") // TODO: Implement
}

func (tgi *testGHClientInterface) UpdateCheckRun(ctx context.Context, id int64, upd github.UpdateCheckRunOptions) (*github.CheckRun, error) {
	panic("not implemented") // TODO: Implement
}

func (tgi *testGHClientInterface) StatusUpdate(ctx context.Context, crID int64, title string, msg string, status string, conclusion string) {
	panic("not implemented") // TODO: Implement
}

func (tgi *testGHClientInterface) CreateCheckRun(ctx context.Context, opts github.CreateCheckRunOptions) (*github.CheckRun, error) {
	panic("not implemented") // TODO: Implement
}

func (tgi *testGHClientInterface) CreateDeployment(ctx context.Context, req *github.DeploymentRequest) (*github.Deployment, error) {
	panic("not implemented") // TODO: Implement
}

func (tgi *testGHClientInterface) CreateDeploymentStatus(ctx context.Context, id int64, req *github.DeploymentStatusRequest) (*github.DeploymentStatus, error) {
	panic("not implemented") // TODO: Implement
}

func (tgi *testGHClientInterface) IsMember(ctx context.Context, user string) (bool, error) {
	panic("not implemented") // TODO: Implement
}

func (tgi *testGHClientInterface) DownloadContents(ctx context.Context, filepath string, opts *github.RepositoryContentGetOptions) (io.ReadCloser, error) {
	panic("not implemented") // TODO: Implement
}

func (tgi *testGHClientInterface) CreateFile(ctx context.Context, filepath string, opts *github.RepositoryContentFileOptions) error {
	panic("not implemented") // TODO: Implement
}

func (tgi *testGHClientInterface) GetContents(ctx context.Context, filepath string, opts *github.RepositoryContentGetOptions) ([]*github.RepositoryContent, error) {
	panic("not implemented") // TODO: Implement
}

func (tgi *testGHClientInterface) GetBranch(ctx context.Context, branch string) (*github.Branch, error) {
	panic("not implemented") // TODO: Implement
}

func (tgi *testGHClientInterface) GetPullRequest(ctx context.Context, prid int) (*github.PullRequest, error) {
	panic("not implemented") // TODO: Implement
}

func (tgi *testGHClientInterface) CreateIssueComment(ctx context.Context, issueID int, opts *github.IssueComment) error {
	panic("not implemented") // TODO: Implement
}

//lint:ignore U1000 this will be used once we need to mock the GH client
type testGHClientSrc struct {
}

func (tcs *testGHClientSrc) getClient(org string, installID int, repo string) (ghClientInterface, error) {
	return &testGHClientInterface{
		instID: 1234,
	}, nil
}
