package main

import (
	"context"
	"io"
	"testing"

	"github.com/google/go-github/v32/github"
)

//lint:ignore U1000 this will be used once we need to mock the GH client
type testGHClientInterface struct {
	src *testGHClientSrc

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

func (tgi *testGHClientInterface) StatusUpdate(ctx context.Context, info *githubInfo, status GithubStatus) {
	if tgi.src.statusUpdates == nil {
		tgi.src.statusUpdates = map[int64][]GithubStatus{}
	}
	tgi.src.statusUpdates[info.checkRunID] = append(tgi.src.statusUpdates[info.checkRunID], status)
}

func (tgi *testGHClientInterface) CreateCheckRun(ctx context.Context, opts github.CreateCheckRunOptions) (*github.CheckRun, error) {
	id := int64(1)
	res := &github.CheckRun{
		Name: github.String(opts.Name),
		ID:   &id,
	}
	tgi.src.addGithubCall("create_check_run", nil, res, opts)
	return res, nil
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

type githubCall struct {
	Args []interface{}
	Res  interface{}
	Err  error
}

type testGHClientSrc struct {
	t *testing.T

	statusUpdates map[int64][]GithubStatus

	actions map[string][]githubCall
}

func (tcs *testGHClientSrc) addGithubCall(call string, err error, res interface{}, args ...interface{}) {
	if tcs.actions == nil {
		tcs.actions = map[string][]githubCall{}
	}
	tcs.actions[call] = append(tcs.actions[call], githubCall{Args: args})
}

func (tcs *testGHClientSrc) getClient(org string, installID int, repo string) (ghClientInterface, error) {
	return &testGHClientInterface{
		instID: 1234,
		src:    tcs,
	}, nil
}

// getCheckRunStatus
func (tcs *testGHClientSrc) getCheckRunStatus() GithubStatus {
	var res GithubStatus
	for _, crss := range tcs.statusUpdates {
		for _, crs := range crss {
			// we should fold the github status updates, appending
			// annotations etc, to mirror github's behaviour

			if crs.Status != "" {
				res.Status = crs.Status
			}
			if crs.Conclusion != "" {
				res.Conclusion = crs.Conclusion
			}
			if crs.Text != "" {
				res.Text = crs.Text
			}
			if crs.DetailsURL != "" {
				res.DetailsURL = crs.DetailsURL
			}
			if crs.Title != "" {
				res.Title = crs.Title
			}
			if crs.Summary != "" {
				res.Summary = crs.Summary
			}

			if crs.Actions != nil {
				res.Actions = crs.Actions
			}

			res.Annotations = append(res.Annotations, crs.Annotations...)

			tcs.t.Logf("crs: %#v", crs)
		}
		// TODO: We'll just pick  random installID for now,
		// tests should be enhanced to cater for different
		// installs.
		return res
	}
	return res
}
