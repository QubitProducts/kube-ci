load("argo.star", "workflow_from_steps", "go_container")

script = '''
set -x
set -e
gofmt -l -e .
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck ./...
go test -v ./...
'''

workflow = workflow_from_steps(
              steps=[go_container(script=script)],
              cacheSize="30Gi")
