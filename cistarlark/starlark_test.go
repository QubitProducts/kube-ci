package cistarlark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"testing"

	argoScheme "github.com/argoproj/argo-workflows/v3/pkg/client/clientset/versioned/scheme"
	"github.com/google/go-github/v45/github"
	"go.starlark.net/starlark"
	"k8s.io/client-go/kubernetes/scheme"
)

func init() {
	if err := argoScheme.AddToScheme(scheme.Scheme); err != nil {
		panic(err)
	}
}

type mockHTTP map[string]string

func response(statusCode int, resp, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Status:     resp,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},

		Body: io.NopCloser(bytes.NewBuffer([]byte(body))),
	}
}

func nilResponse(statusCode int) *http.Response {
	text := http.StatusText(statusCode)
	json := fmt.Sprintf(`{"message": %q}`, text)
	resp := response(statusCode, text, json)
	return resp
}

func githubContentURL(org, repo, branch, filename string) string {
	ustr := fmt.Sprintf(`https://api.github.com/repos/%s/%s/contents/%s?ref=%s`, org, repo, filename, branch)
	u, _ := url.Parse(ustr)
	return u.String()
}

func (m *mockHTTP) contentAdd(org, repo, branch, dir, data string) {
	u := githubContentURL(org, repo, branch, dir)
	log.Printf("adding %s", u)
	(*m)[u] = data
}

func (m *mockHTTP) contentAddGithubContent(org, repo, branch, dir string, data map[string]string) {
	for fn, v := range data {
		rci := &github.RepositoryContent{}
		rci.Name = github.String(fn)
		rci.Path = github.String(dir + "/" + fn)
		rci.Content = github.String(v)

		bs, err := json.Marshal(rci)
		if err != nil {
			panic("couldn't render content, %v")
		}

		m.contentAdd(org, repo, branch, *rci.Path, string(bs))
	}

}

func (m *mockHTTP) RoundTrip(req *http.Request) (*http.Response, error) {
	log.Printf("roundtrip: %#v", req.URL.String())
	if req.Method != http.MethodGet {
		log.Printf("roundtrip: bad method")
		return nilResponse(http.StatusMethodNotAllowed), nil
	}
	data, ok := (*m)[req.URL.String()]
	if !ok {
		log.Printf("roundtrip: not found")
		return nilResponse(404), nil
	}

	log.Printf("data: %s", data)
	return response(200, http.StatusText(200), data), nil
}

func TestStarlark(t *testing.T) {
	mock := &mockHTTP{}
	mock.contentAddGithubContent(
		"myorg",
		"myrepo2",
		"testpr",
		"/testdata",
		map[string]string{
			"thing.star": `data = ["things", "here"]`,
		},
	)

	testStar := `
load("github://myrepo2.myorg/testdata/thing.star?ref=testpr", "data")
working = data
		`
	const script = `
load("builtin:///re", "re")
load("builtin:///encoding/yaml", "yaml")

load("builtin:///encoding/json", "decode")
load("/test.star", "working")
print("hello, world")

myre = re.compile("thing(.+)")
match = myre.match("thing-something")

squares = [x*x for x in range(10)]

jsonStr = loadFile("/test.json")
json = decode(jsonStr)

print(input)

# workflow = yaml.loads(loadFile("/ci.yaml"))
entrypoint = "test"
workflow = {
		"apiVersion": "argoproj.io/v1alpha1",
		"kind": "Workflow",
		"spec": {
				"arguments": {},
				"entrypoint": entrypoint,
				"templates": [ { "name": "test"} ] } }
`

	ciYaml := `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  arguments: {}
  entrypoint: test
  templates:
  - name: test
`
	ciContextData := map[string]string{
		"/ci.star":   script,
		"/ci.yaml":   ciYaml,
		"/test.star": testStar,
		"/test.json": `{"hello": "world"}`,
	}

	client := &http.Client{Transport: mock}
	ciCtx := WorkflowContext{
		ContextData: ciContextData,
		Ref:         "master",
		RefType:     "branch",
		SHA:         "1234",
		Repo: &github.Repository{
			Name: github.String("myrepo"),
			Organization: &github.Organization{
				Name: github.String("myorg"),
			},
		},
	}

	cfg := Config{
		Print: func(_ *starlark.Thread, msg string) { t.Logf("message: %s", msg) },
	}

	wf, err := LoadWorkflow(context.Background(), client, "/ci.star", ciCtx, cfg)

	if err != nil {
		t.Fatalf("LoadWorkflow failed, %v", err)
	}

	bs, _ := json.Marshal(wf)
	t.Logf("wf:\n%s", bs)
}
