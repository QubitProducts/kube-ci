# print(input)

load("argo.star", "argoWorkflow")

spec = {
    "entrypoint": "test",
    "templates": [
      {
        "name": "test",
        "steps": [
          [
            {
              "name": "test",
              "template": "test-node"
            }
          ]
        ]
      },
      {
        "container": {
          "args": [
            "sh",
            "-c",
            "set -x\nset -e\ngofmt -l -e .\ngo vet ./...\ngo run honnef.co/go/tools/cmd/staticcheck ./...\ngo test -v ./...\n"
          ],
          "command": [
            "/bin/sh",
            "-c"
          ],
          "env": [
            {
              "name": "GOPROXY",
              "value": "http://gomod-athens:8080"
            }
          ],
          "image": "golang:1.19",
          "workingDir": "/src"
        },
        "inputs": {
          "artifacts": [
            {
              "git": {
                "repo": "{{workflow.parameters.repo}}",
                "revision": "{{workflow.parameters.revision}}",
                "sshPrivateKeySecret": {
                  "key": "ssh-private-key",
                  "name": "ci-secrets"
                }
              },
              "name": "code",
              "path": "/src"
            }
          ]
        },
        "name": "test-node"
      }
    ],
  }

# The future starts here!

workflow = argoWorkflow(
              spec=spec,
              cacheSize="20Gi")
