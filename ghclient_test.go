package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"testing"

	"github.com/google/go-github/v45/github"
)

type repoContentKey struct {
	org  string
	repo string
	ref  string
}

type repoFiles map[string]string
type repoContent map[repoContentKey]repoFiles

func (c *repoContent) add(org, repo, ref, fileName, fileContent string) {
	key := repoContentKey{
		org:  org,
		repo: repo,
		ref:  ref,
	}
	cnt := (*c)[key]
	if cnt == nil {
		cnt = repoFiles{}
	}
	cnt[fileName] = fileContent
	(*c)[key] = cnt
}

type testGHClientInterface struct {
	instID int
	org    string
	repo   string

	src *testGHClientSrc
	t   *testing.T
}

func (tgi *testGHClientInterface) GetInstallID() int {
	return tgi.instID
}

func (tgi *testGHClientInterface) GetRef(ctx context.Context, ref string) (*github.Reference, error) {
	panic("not implemented") // TODO: Implement
}

func (tgi *testGHClientInterface) CreateCheckRun(ctx context.Context, opts github.CreateCheckRunOptions) (*github.CheckRun, error) {
	id := int64(1)
	res := &github.CheckRun{
		Name:       github.String(opts.Name),
		ID:         &id,
		DetailsURL: opts.DetailsURL,
		HeadSHA:    &opts.HeadSHA,
		Conclusion: opts.Conclusion,
		Status:     opts.Status,
		Output:     opts.Output,
	}
	tgi.src.addGithubCall("create_check_run", nil, res, opts)
	return res, nil
}

func (tgi *testGHClientInterface) UpdateCheckRun(ctx context.Context, id int64, upd github.UpdateCheckRunOptions) (*github.CheckRun, error) {
	// we never actually use the result of UpdateCheckRUn, so
	// this can be ignored
	res := &github.CheckRun{
		Name: github.String(upd.Name),
		ID:   &id,
	}

	tgi.src.addGithubCall("update_check_run", nil, res, id, upd)

	return res, nil
}

func (tgi *testGHClientInterface) CreateDeployment(ctx context.Context, req *github.DeploymentRequest) (*github.Deployment, error) {
	id := int64(1)
	res := &github.Deployment{
		ID: &id,
	}
	tgi.src.addGithubCall("create_deployment", nil, res, req)
	return res, nil
}

func (tgi *testGHClientInterface) CreateDeploymentStatus(ctx context.Context, id int64, req *github.DeploymentStatusRequest) (*github.DeploymentStatus, error) {
	res := &github.DeploymentStatus{
		State:  req.State,
		LogURL: req.LogURL,
	}
	tgi.src.addGithubCall("create_deployment_status", nil, res, req)
	return res, nil
}

func (tgi *testGHClientInterface) IsMember(ctx context.Context, user string) (bool, error) {
	panic("not implemented") // TODO: Implement
}

func (tgi *testGHClientInterface) DownloadRepoContents(ctx context.Context, filepath string, opts *github.RepositoryContentGetOptions) (io.ReadCloser, error) {
	return tgi.DownloadContents(ctx, tgi.org, tgi.repo, filepath, opts)
}

func (tgi *testGHClientInterface) DownloadContents(ctx context.Context, org, repo, filepath string, opts *github.RepositoryContentGetOptions) (io.ReadCloser, error) {
	key := repoContentKey{
		org:  org,
		repo: repo,
		ref:  opts.Ref,
	}
	repoCnt, ok := tgi.src.content[key]
	if !ok {
		err := fmt.Errorf("can't find ref %s/%s %s, %w", key.org, key.repo, key.ref, os.ErrNotExist)
		tgi.src.addGithubCall("get_contents", err, nil, key.org, key.repo, opts)
		return nil, err
	}

	str, ok := repoCnt[filepath]
	if !ok {
		err := fmt.Errorf("file not in org/repo, %w", os.ErrNotExist)
		tgi.src.addGithubCall("get_contents", err, nil, key.org, key.repo, opts)
		return nil, err
	}

	res := ioutil.NopCloser(bytes.NewBufferString(str))
	tgi.src.addGithubCall("get_contents", nil, res, key.org, key.repo, opts)
	return res, nil
}

func (tgi *testGHClientInterface) CreateFile(ctx context.Context, filepath string, opts *github.RepositoryContentFileOptions) error {
	panic("not implemented") // TODO: Implement
}

func (tgi *testGHClientInterface) GetRepoContents(ctx context.Context, filepath string, opts *github.RepositoryContentGetOptions) ([]*github.RepositoryContent, error) {
	key := repoContentKey{
		org:  tgi.org,
		repo: tgi.repo,
		ref:  opts.Ref,
	}
	repoCnt, ok := tgi.src.content[key]
	if !ok {
		err := fmt.Errorf("could not find file, %w", os.ErrNotExist)
		tgi.src.addGithubCall("get_contents", err, nil, tgi.org, tgi.repo, opts)
		return nil, err
	}

	str, ok := repoCnt[filepath]
	if !ok {
		err := fmt.Errorf("could not find file, %w", os.ErrNotExist)
		tgi.src.addGithubCall("get_contents", err, nil, tgi.org, tgi.repo, opts)
		return nil, err
	}

	res := []*github.RepositoryContent{
		{
			Name:    github.String(filepath),
			Content: github.String(str),
		},
	}
	tgi.src.addGithubCall("get_contents", nil, res, tgi.org, tgi.repo, opts)

	return res, nil
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

	content repoContent
	calls   map[string][]githubCall
}

func (tcs *testGHClientSrc) addGithubCall(call string, err error, res interface{}, args ...interface{}) {
	if tcs.calls == nil {
		tcs.calls = map[string][]githubCall{}
	}
	ghcall := githubCall{Args: args, Err: err, Res: res}
	tcs.calls[call] = append(tcs.calls[call], ghcall)
}

func (tcs *testGHClientSrc) getClient(org string, installID int, repo string) (ghClientInterface, error) {
	return &testGHClientInterface{
		instID: 1234,
		org:    org,
		repo:   repo,
		src:    tcs,
	}, nil
}

// getCheckRunStatus calculates the check run status based on the
// mechanism that github seems to use (infered by poking the API)
// - Updates must always specify name, or it is blanked.
// - If Output is set, all the fields are updated.
// - Other fields are optional (with the documented behaviour of status,
//   completed, and completedAt
// The mock currently assumed there is only one org, repo and install
func (tcs *testGHClientSrc) getCheckRunStatus(id int64) (github.CheckRun, []*github.CheckRunAction) {
	createCalls := tcs.calls["create_check_run"]
	updateCalls := tcs.calls["update_check_run"]

	checkRuns := map[int64]github.CheckRun{}
	checkRunsActions := map[int64][]*github.CheckRunAction{}
	for _, call := range createCalls {
		res := call.Res.(*github.CheckRun)
		checkRuns[res.GetID()] = *res
		opts := call.Args[0].(github.CreateCheckRunOptions)
		checkRunsActions[res.GetID()] = opts.Actions
	}

	res := checkRuns[id]
	out := &github.CheckRunOutput{}
	resActions := checkRunsActions[id]

	// Need to copy output
	if res.Output != nil {
		*out = *res.Output
	}
	res.Output = out
	res.ID = nil

	for _, call := range updateCalls {
		crid := call.Args[0].(int64)
		if crid != id {
			continue
		}

		upd := call.Args[1].(github.UpdateCheckRunOptions)
		if upd.Actions != nil {
			resActions = upd.Actions
		}

		if upd.DetailsURL != nil {
			res.DetailsURL = upd.DetailsURL
		}
		if upd.Conclusion != nil {
			res.Conclusion = upd.Conclusion
		}
		if upd.Status != nil {
			res.Status = upd.Status
		}

		res.Name = &upd.Name
		if upd.Output != nil {
			res.Output.Text = upd.Output.Text
			res.Output.Title = upd.Output.Title
			res.Output.Summary = upd.Output.Summary
			res.Output.Images = append(res.Output.Images, upd.Output.Images...)
			res.Output.Annotations = append(res.Output.Annotations, upd.Output.Annotations...)
		}
	}

	return res, resActions
}
