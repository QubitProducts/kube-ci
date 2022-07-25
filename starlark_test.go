package main

import (
	"net/http"
	"net/url"
	"testing"

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

func TestStarlark(t *testing.T) {
	yaml, _ := starlibyaml.LoadModule()
	re, _ := starlibre.LoadModule()

	builtins := map[string]starlark.StringDict{
		"/" + starlibyaml.ModuleName: yaml,
		"/" + starlibjson.ModuleName: starlibjson.Module.Members,
		"/" + starlibre.ModuleName:   re,
	}

	clients := &testGHClientSrc{t: t}
	client, err := clients.getClient("myorg", 1234, "myrepo")
	if err != nil {
		t.Fatal(err)
	}

	clients.content = repoContent{}
	clients.content.add("myorg", "myrepo2", "testpr", "/testdata/thing.star", `data = ["things", "here"]`)

	testStar := `
load("//myrepo2.myorg/testdata/thing.star?ref=testpr", "data")
working = data
`
	clients.content.add("myorg", "myrepo", "mypr", "/.kube-ci/ci.yaml", ``)
	clients.content.add("myorg", "myrepo", "mypr", "/.kube-ci/test.star", testStar)
	clients.content.add("myorg", "myrepo", "mypr", "/.kube-ci/test.json", `{"hello": "world"}`)

	t.Logf("stuff: %#v", clients.content)

	src := &modSrc{
		client:  client,
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
	u, _ := url.Parse("github://myrepo.myorg/.kube-ci/ci.star?ref=mypr")
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
