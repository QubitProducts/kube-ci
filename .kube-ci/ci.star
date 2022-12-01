workflow = {
  "apiVersion": "argoproj.io/v1alpha1",
  "kind": "Workflow",
  "metadata": {
    "annotations": {
      "kube-ci.qutics.com/cacheScope": "project",
      "kube-ci.qutics.com/cacheSize": "20Gi"
    }
  },
  "spec": {
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
          "volumeMounts": [
            {
              "mountPath": "/cache",
              "name": "build-cache"
            }
          ],
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
    "volumes": [
      {
        "name": "build-cache",
        "persistentVolumeClaim": {
          "claimName": "{{workflow.parameters.cacheVolumeClaimName}}"
        }
      }
    ]
  }
}
