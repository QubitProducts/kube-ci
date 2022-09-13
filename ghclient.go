package kubeci

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	workflow "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	"github.com/google/go-github/v45/github"
	"k8s.io/client-go/kubernetes/scheme"
)

type repoClient struct {
	installID int
	org       string
	repo      string

	client *github.Client
}

func (r *repoClient) GetInstallID() int {
	return r.installID
}

func (r *repoClient) GetRef(ctx context.Context, ref string) (*github.Reference, error) {
	gref, _, err := r.client.Git.GetRef(
		ctx,
		r.org,
		r.repo,
		ref,
	)
	return gref, err
}

func (r *repoClient) GetRepo(ctx context.Context) (*github.Repository, error) {
	res, _, err := r.client.Repositories.Get(
		ctx,
		r.org,
		r.repo,
	)
	return res, err
}

func (r *repoClient) UpdateCheckRun(ctx context.Context, id int64, upd github.UpdateCheckRunOptions) (*github.CheckRun, error) {
	cr, _, err := r.client.Checks.UpdateCheckRun(
		ctx,
		r.org,
		r.repo,
		id,
		upd,
	)
	return cr, err
}

func StatusUpdate(
	ctx context.Context,
	info *githubInfo,
	status GithubStatus,
) {
	opts := github.UpdateCheckRunOptions{
		// Mandatory on update
		Name: info.checkRunName,
	}

	// These are optional
	if status.DetailsURL != "" {
		opts.DetailsURL = &status.DetailsURL
	}

	if status.Status != "" {
		opts.Status = &status.Status
	}

	if status.Conclusion != "" {
		opts.Conclusion = &status.Conclusion
		opts.CompletedAt = &github.Timestamp{
			Time: time.Now(),
		}
	}

	// If any of these are set we update the output,
	// so all must be set
	if status.Title != "" ||
		status.Summary != "" ||
		status.Text != "" {
		opts.Output = &github.CheckRunOutput{
			Title:       &status.Title,
			Summary:     &status.Summary,
			Text:        &status.Text,
			Annotations: status.Annotations,
		}
	}

	opts.Actions = status.Actions

	_, err := info.ghClient.UpdateCheckRun(
		ctx,
		info.checkRunID,
		opts)

	if err != nil {
		log.Printf("Update of aborted check run failed, %v", err)
	}
}

func (r *repoClient) CreateCheckRun(ctx context.Context, opts github.CreateCheckRunOptions) (*github.CheckRun, error) {
	cr, _, err := r.client.Checks.CreateCheckRun(ctx,
		r.org,
		r.repo,
		opts,
	)
	return cr, err
}

func (r *repoClient) CreateDeployment(ctx context.Context, req *github.DeploymentRequest) (*github.Deployment, error) {
	dep, _, err := r.client.Repositories.CreateDeployment(
		ctx,
		r.org,
		r.repo,
		req,
	)
	return dep, err
}

func (r *repoClient) CreateDeploymentStatus(ctx context.Context, id int64, req *github.DeploymentStatusRequest) (*github.DeploymentStatus, error) {
	dep, _, err := r.client.Repositories.CreateDeploymentStatus(
		ctx,
		r.org,
		r.repo,
		id,
		req,
	)
	return dep, err
}

func (r *repoClient) IsMember(ctx context.Context, user string) (bool, error) {
	ok, _, err := r.client.Organizations.IsMember(
		ctx,
		r.org,
		user,
	)
	return ok, err
}

func (r *repoClient) DownloadRepoContents(ctx context.Context, filepath string, opts *github.RepositoryContentGetOptions) (io.ReadCloser, error) {
	return r.DownloadContents(
		ctx,
		r.org,
		r.repo,
		filepath,
		opts,
	)
}

func (r *repoClient) DownloadContents(ctx context.Context, org, repo, filepath string, opts *github.RepositoryContentGetOptions) (io.ReadCloser, error) {
	rc, _, err := r.client.Repositories.DownloadContents(
		ctx,
		org,
		repo,
		filepath,
		opts,
	)
	return rc, err
}

func (r *repoClient) GetRepoContents(ctx context.Context, filepath string, opts *github.RepositoryContentGetOptions) ([]*github.RepositoryContent, error) {
	_, files, _, err := r.client.Repositories.GetContents(
		ctx,
		r.org,
		r.repo,
		filepath,
		opts,
	)
	return files, err
}

func (r *repoClient) CreateFile(ctx context.Context, filepath string, opts *github.RepositoryContentFileOptions) error {
	_, _, err := r.client.Repositories.CreateFile(ctx, r.org, r.repo, filepath, opts)
	return err
}

func (r *repoClient) GetBranch(ctx context.Context, branch string) (*github.Branch, error) {
	gbranch, _, err := r.client.Repositories.GetBranch(ctx, r.org, r.repo, branch, true)
	return gbranch, err
}

func (r *repoClient) GetPullRequest(ctx context.Context, prid int) (*github.PullRequest, error) {
	pr, _, err := r.client.PullRequests.Get(ctx, r.org, r.repo, prid)
	return pr, err
}

func (r *repoClient) CreateIssueComment(ctx context.Context, issueID int, opts *github.IssueComment) error {
	_, _, err := r.client.Issues.CreateComment(
		ctx,
		r.org,
		r.repo,
		issueID,
		opts)
	return err
}

type contentDownloader interface {
	DownloadRepoContents(ctx context.Context, filepath string, opts *github.RepositoryContentGetOptions) (io.ReadCloser, error)
}

func getFile(
	ctx context.Context,
	cd contentDownloader,
	sha string,
	filename string) (io.ReadCloser, error) {

	file, err := cd.DownloadRepoContents(
		ctx,
		filename,
		&github.RepositoryContentGetOptions{
			Ref: sha,
		})
	if err != nil {
		if ghErr, ok := err.(*github.ErrorResponse); ok {
			if ghErr.Response.StatusCode == http.StatusNotFound {
				return nil, os.ErrNotExist
			}
		}
		return nil, err
	}
	return file, nil
}

func getWorkflow(
	ctx context.Context,
	cd contentDownloader,
	sha string,
	filename string) (*workflow.Workflow, error) {

	file, err := getFile(
		ctx,
		cd,
		sha,
		filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	bs := &bytes.Buffer{}

	_, err = io.Copy(bs, file)
	if err != nil {
		return nil, err
	}

	decode := scheme.Codecs.UniversalDeserializer().Decode
	obj, _, err := decode(bs.Bytes(), nil, nil)

	if err != nil {
		return nil, fmt.Errorf("failed to decode %s, %v", filename, err)
	}

	wf, ok := obj.(*workflow.Workflow)
	if !ok {
		return nil, fmt.Errorf("could not use %T as workflow", wf)
	}

	return wf, nil
}
