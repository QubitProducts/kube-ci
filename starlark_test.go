package main

import (
	"net/http"
	"net/url"
	"os"
	"testing"

	"github.com/google/go-github/v45/github"
	starlibjson "github.com/qri-io/starlib/encoding/json"
	starlibyaml "github.com/qri-io/starlib/encoding/yaml"
	starlibre "github.com/qri-io/starlib/re"
	"go.starlark.net/starlark"
)

type userRT struct {
	user  string
	token string
	next  http.RoundTripper
}

func (u *userRT) RoundTrip(req *http.Request) (*http.Response, error) {
	nreq := req.Clone(req.Context())
	nreq.SetBasicAuth(u.user, u.token)

	resp, err := u.next.RoundTrip(nreq)
	return resp, err
}

func TestStarlark(t *testing.T) {
	yaml, _ := starlibyaml.LoadModule()
	re, _ := starlibre.LoadModule()

	builtins := map[string]starlark.StringDict{
		"/" + starlibyaml.ModuleName: yaml,
		"/" + starlibjson.ModuleName: starlibjson.Module.Members,
		"/" + starlibre.ModuleName:   re,
	}

	httpClient := http.DefaultClient
	ut := &userRT{
		user:  "tcolgate",
		token: os.Getenv("GITHUB_TOKEN"),
		next:  http.DefaultTransport,
	}
	httpClient.Transport = ut

	gh := github.NewClient(httpClient)
	src := &modSrc{
		http:    httpClient,
		client:  gh.Repositories,
		builtIn: builtins,
	}

	// This dictionary defines the pre-declared environment.
	predeclared := starlark.StringDict{
		"workflow": starlark.String("world"),
		"loadFile": starlark.NewBuiltin("loadFile", src.builtInLoadFile),
	}
	src.predeclared = predeclared

	loader := MakeLoad(src)

	// The Thread defines the behavior of the built-in 'print' function.
	thread := &starlark.Thread{
		Name:  "main",
		Print: func(_ *starlark.Thread, msg string) { t.Logf(msg) },
		Load:  loader,
	}
	u, _ := url.Parse("github://kube-ci.QubitProducts/testdata/?ref=starlark")
	setCWU(thread, u)

	const script = `
load("builtin:///re.star", "re")
load("builtin:///encoding/json.star", "decode")
load("builtin:///encoding/yaml.star", "yaml")
load("test.star", "working")
print("hello, world")

myre = re.compile("thing(.+)")
match = myre.match("thing-something")

squares = [x*x for x in range(10)]
workflow = {"something": "something"}
workflow["working"] =  working

jsonStr = loadFile("test.json")
json = decode(jsonStr)

wf = yaml.loads(loadFile("/.kube-ci/ci.yaml"))

`
	val, err := starlark.ExecFile(thread, "main", script, predeclared)
	if err != nil {
		t.Fatalf("err from starlark, %v", err)
	}
	t.Logf("val: %s", val)
}
