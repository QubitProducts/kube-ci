# Github CI with K8S and Argo.

kube-ci builds on argo, and the github API, to provide CI/CD workflows. The
design goal is to provide as much feedback and support directly via the github
API, rather than through other interfaces.

# What's here
The existing code is functional, though quality needs improving. The existing
application:

If a given commit has a workflow file, location in ```.kube-ci/ci.yaml```,
kube-ci:
- Creates a Check Run when a Check Suite is requested. (see Build Policy below)
- Creates the a workflow based on the provided file.
- Reports parsing errors of the user supplied workflow back the Github UI.
- Feeds back the status of Workflow pods to the Github UI
- Links to the argo-ui for the created workflow
- Populates `file:N:M: message` from logs as Annotations in GitHub check runs.
- You can include configurations which can be imported by comments on github
  PRs/issues
- Optionally create a PVC, either per-repo or per-repo-branch, that can be used
  for cacheing between runs. (clean up of these is yet to be implemented)
- Allows workflows runs to be manually trigger from a PR using `/kube-ci` commands.
- Allows initial bootstrap of the workflows via templates and a `/kube-ci` command from
  issues and PRs.
- Allows filtering responses to limit them to specific orgs and install IDs

## Build Policy

If a repo contains the require kube-ci files, then a build will be triggered if:

- A commit to a branches matching a configurable regexp is pushed (specifically when a CheckSuite even is sent). (Unless runForBranch is set to "false")
- A new tag is created (only if the "runForTag" annotation is set to "true")
- A non-Draft PR is raised between branches in the existing repository. (a configuration
  option allows building of Draft PR branches too).  - If a PR is from a remote repository a manual run must be triggered using /kube-ci.
- If `/kube-ci run` command is issued by a member of an org that matches a configurable
  regexp.

## Workflow Annotations.

You can use annotations to control a few aspects of workflow execution.

Selective running:
  *kube-ci.qutics.com/runForBranch*: defaults to "true", runs for branches (specifically triggered from github CheckRun events when commits are pushed)
  *kube-ci.qutics.com/runForTag*: defaults to "false", runs when a tag is created.

Cache volume:
- *kube-ci.qutics.com/cacheScope*: "project" or "branch", whether the volume is create per
  github project, or per branch. Branch cache volumes are deleted when the branch is deleted.
  All related volumes are deleted when a project is deleted or archived.
- *kube-ci.qutics.com/cacheSize*: e.g. "20Gi", this can be used to set the size of the volume.
- *kube-ci.qutics.com/cacheStorageClassName*: override the storage class name used for creating the volume
- *kube-ci.qutics.com/cacheName*: This can be used to set a specific PVC claim name to be used as the cache,
  other cache settings will be ignored.

Manual step, and deployment controls:
- kube-ci.qutics.com/manualTemplates: override the set of templates that will
  be presented as manually run check-run steps.
- kube-ci.qutics.com/essentialTemplates: override the set of manual templates that will
  be fail the check until run (they are marked as conclusion `action_required`)
- kube-ci.qutics.com/deployTemplates: override the set of templates that will
  be tracked via a GitHub deployment.
- kube-ci.qutics.com/nonInteractiveBranches: Do not present manual check-run
  steps for check-run's created for commits with this as a the head branch.
  (these are in addition to check-runs created with the repositories default
  branch as the head branch.

## Workflow Parameters.

Some extra parameteres will be added to your workflow before it is run.

- *repo*: git@github.com:yourorg/repo.git
- *repo_git_url*: git//github.com/yourorg/repo.git
- *repo_https_url*: http://github.com/yourorg/repo.git
- *repoName*: repo
- *orgName*: yourorg
- *revision*: 01245789abc.....
- *refType*: "branch" or "tag"
- *refName*: someref (regardless of branch or tag)
- *branch*: newpr5 (the head branch) (only if this is a branch)
- *tag*: v1.0.1 (tag if this is from a tag push or create)
- *cacheVolumeClaimName*: the name of the pvc created (if requested)
- *repoDefaultBranch*: the default branch configured on the repository

Workflows run for pull-requests will get this additional parameters:

- *pullRequestID*: "123"
- *pullRequestBaseBranch*: prbranch (The base branch being PR'd to

## Workflow volumes.

You can create a volume to be used with a workflow by adding some annotations to your
workflow.

```
annotations:
  "kube-ci.qutics.com/cacheScope": "project"
  "kube-ci.qutics.com/cacheSize": "20Gi"
```

## TODO
- better metrics
- example deployment assets

## Manual Check Run steps

On successful completion of the default template of your workflow, kube-ci can present an extra set
of check-runs, set to "neutral". The user can then run, or skip those steps using buttons on the
check-run. If the user clicks "Run", a new argo-workflow will be run for that step.

Manual check run steps default to "neutral" conclusion, which present as grey, and will not mark the check
as failing. If you want to force a given template to be run, you can set the essentialTemplates configuration
, or override the value using the workflow annotation.

Check runs are not presented in the following circumstances:
- If the default entrypoint workflow fails
- If the workflow that has run is not the default entrypoint workflow.
- If the workflow head branch is the same as the repo default branch
- If the workflow head branch matches the configured non-interactive-branches regular expression.

The last two steps are intended to prevent "failing" check-runs created for merge commits for a
successfully merged PR (the logic here may need to change in future).

## GitHub Deployments

Kube-CI can create Github Deployments to track which commits are deployed to which environments.
Any step that includes a template that matches the configure DeploymentTemplates regular expression
(or workflow annotation override), will have a Deployment created and tracked, based on the
success/failure of the run of that deployment.

The template must include a parameter that passes in the specific environment
to create the Deployment for. By default we look for `environment` in the parameters
list, but this can be overriden with the `environmentParameter` configuration variable.

# Deploying

- Pick a URL for the webhook.The Kube-CI github WebHook needs to be reachable
  publicly by GitHub. e.g. at ``` https://kube-ci.example.com/webhooks/github```

- Create a GitHub application
  - Set a webhook secret, keep a note of it.
  - Note the AppID.
  - Generate a private key. Download it somewhere.
  - Install the App on one of more accounts, you will need to take a note
    of the Installation ID (this shows up in the URL of the settings for the
    installation e.g.
    ```https://github.com/organizations/YOURORG/settings/installations/12345```
  - It need Read permission to repos and repo contents, and read/write to
    Checks.
  - Enable the following events (TODO: not all of these are used, needs review):
    - Check suite
    - Check run
    - Push
    - Create
    - Delete
    - Commit Comment
    - Deployment
    - Deployment Status
    - Issue Comment
    - Pull Request
    - Pull Request Review
    - Release
    - Repository

- Deploy kube-ci
  - You should already have argo deployed. Some things currently assume a
    namespace of "argo" exists for deploying the workflows to.
  - I've only tested this in cluster, but the flags are there for providing
    a kube-config if you want one.
  - You may need to add RBAC rules along the lines of those in `rbac.yml`
  - Setup a service/ingress for the webhook.
  - The follow options can be configured
    - `-github.appid=1234`
    - `-config=/file.yml`: path to yaml config file with some workflow defaults
    - `-keyfile=/keyfile.pem`: path to the secret key
    - `-secretfile=/webhook-secret": path to a file containing the webhook secret
    - "-argo.ui.base=https://kube-ci.example.com": external URL of your argo UI.
    - "-idsfile=/config/ids": not required, but limits the app to respond only
      to events for specific install IDs. One ID per line

- At present we deploy kube-ci behind an oauth2 proxy that passes the require webhook URLs
  through, but requires auth for everything else using the github app info. All other traffic
  is proxied to the argo-ui. In future this will be baked in to kube-ci directly.



