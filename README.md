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

- A commit to a branches matching a configurable regexp is pushed.
- A non-Draft PR is raised between branches in the existing repository. (a configuration
  option allows building of Draft PR branches too).  - If a PR is from a remote repository a manual run must be triggered using /kube-ci.
- If `/kube-ci run` command is issued by a member of an org that matches a configurable
  regexp.

## Workflow Parameters.

Some extra parameteres will be added to your workflow before it is run.

- *repo*: git@github.com:yourorg/repo.git
- *repo_git_url*: git//github.com/yourorg/repo.git
- *repo_https_url*: http://github.com/yourorg/repo.git
- *repoName*: repo
- *orgName*: yourorg
- *revision*: 01245789abc.....
- *branch*: newpr5 (the head branch)
- *cacheVolumeClaimName*: the name of the pvc created (if requested)

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
    - Create
    - Delete
    - Commit Comment
    - Deployment
    - Deployment Status
    - Issue Comment
    - Pull Request
    - Pull Request Review
    - Push
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



