package cistarlark

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/go-github/v45/github"
	starlibjson "github.com/qri-io/starlib/encoding/json"
	starlibyaml "github.com/qri-io/starlib/encoding/yaml"
	starlibre "github.com/qri-io/starlib/re"
	"go.starlark.net/starlark"
)

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
	yaml, _ := starlibyaml.LoadModule()
	re, _ := starlibre.LoadModule()

	builtins := map[string]starlark.StringDict{
		"/" + starlibyaml.ModuleName: yaml,
		"/" + starlibjson.ModuleName: starlibjson.Module.Members,
		"/" + starlibre.ModuleName:   re,
	}

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

	/*
		mock.contentAddGithubContent(
			"myorg",
			"myrepo",
			"mypr",
			"/.kube-ci",
			map[string]string{
				"ci.yaml":   ``,
				"test.star": testStar,
				"test.json": `{"hello": "world"}`,
			},
		)
	*/

	ciContext := map[string]string{
		"/ci.yaml":   ``,
		"/test.star": testStar,
		"/test.json": `{"hello": "world"}`,
	}

	src := newModSource(&http.Client{Transport: mock})

	predeclared := starlark.StringDict{
		"workflow": starlark.String("world"),
		"loadFile": starlark.NewBuiltin("loadFile", src.builtInLoadFile),
	}

	// This dictionary defines the pre-declared environment.
	src.SetPredeclared(predeclared)
	src.SetBuiltIn(builtins)
	src.SetContext(ciContext)

	loader := MakeLoad(src)

	// The Thread defines the behavior of the built-in 'print' function.
	thread := &starlark.Thread{
		Name:  "main",
		Print: func(_ *starlark.Thread, msg string) { t.Logf(msg) },
		Load:  loader,
	}
	u, _ := url.Parse("context:///ci.star")
	setCWU(thread, u)

	const script = `
load("builtin:///re.star", "re")
load("builtin:///encoding/json.star", "decode")
load("builtin:///encoding/yaml.star", "yaml")
load("/test.star", "working")
print("hello, world")

myre = re.compile("thing(.+)")
match = myre.match("thing-something")

squares = [x*x for x in range(10)]
workflow = {"something": "something"}
workflow["working"] =  working

jsonStr = loadFile("/test.json")
json = decode(jsonStr)

wf = yaml.loads(loadFile("/ci.yaml"))

`
	val, err := starlark.ExecFile(thread, "main", script, src.predeclared)
	if err != nil {
		t.Fatalf("err from starlark, %v", err)
	}
	t.Logf("val: %s", val)
}
