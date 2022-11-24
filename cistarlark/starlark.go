// Copyright 2017 The Bazel Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cistarlark

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/google/go-github/v45/github"
	starlibjson "github.com/qri-io/starlib/encoding/json"
	starlibyaml "github.com/qri-io/starlib/encoding/yaml"
	starlibre "github.com/qri-io/starlib/re"
	"go.starlark.net/starlark"
	"k8s.io/client-go/kubernetes/scheme"

	workflow "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
)

type githubContentGetter interface {
	GetContents(ctx context.Context, owner, repo, path string, opts *github.RepositoryContentGetOptions) (fileContent *github.RepositoryContent, directoryContent []*github.RepositoryContent, resp *github.Response, err error)
}

type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

type modOpener interface {
	ModOpen(*starlark.Thread, string) (starlark.StringDict, error)
}

var (
	emptyStr = starlark.String("")
)

func setCWU(thread *starlark.Thread, u *url.URL) {
	thread.SetLocal(workingURL, u)
}

func getCWU(thread *starlark.Thread) *url.URL {
	ui := thread.Local(workingURL)
	if ui == nil {
		thread.Cancel("working URL not set")
		return nil
	}
	return ui.(*url.URL)
}

type modSrc struct {
	builtIn     map[string]starlark.StringDict
	predeclared starlark.StringDict
	context     map[string]string

	client githubContentGetter
	http   httpDoer
}

func newModSource(http *http.Client) *modSrc {

	ghc := github.NewClient(http)

	return &modSrc{
		client: ghc.Repositories,
		http:   http,
	}
}

func (gh *modSrc) SetBuiltIn(builtIn map[string]starlark.StringDict) {
	gh.builtIn = builtIn
}

func (gh *modSrc) SetPredeclared(predeclared starlark.StringDict) {
	gh.predeclared = predeclared
}

func (gh *modSrc) SetContext(cntx map[string]string) {
	gh.context = cntx
}

func validateModURL(u *url.URL) error {
	var err error
	switch u.Scheme {
	case "github":
		if len(strings.Split(u.Host, ".")) != 2 {
			return fmt.Errorf("invalid host %q for github scheme, should be empty, repo, or org.repo", u.Host)
		}
	case "builtin":
		if u.Host != "" {
			return fmt.Errorf("host is not invalid for the builtin scheme")
		}
	case "context":
		if u.Host != "" {
			return fmt.Errorf("host is not invalid for the context scheme")
		}
	case "http":
	}

	return err
}

func (gh *modSrc) builtInLoadFile(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var fn string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "fn", &fn); err != nil {
		return nil, err
	}

	cwu := getCWU(thread)
	u, err := parseModURL(cwu, fn)
	if err != nil {
		return nil, err
	}

	var res starlark.String
	loaders := map[string]func(*starlark.Thread, *url.URL) (starlark.String, error){
		"github":  gh.openGithubFile,
		"https":   gh.openHTTPFile,
		"context": gh.openContextFile,
	}

	loader, ok := loaders[u.Scheme]
	if !ok {
		return nil, fmt.Errorf("unsupported scheme, %s", u.Scheme)
	}

	res, err = loader(thread, u)
	if err != nil {
		return nil, fmt.Errorf("could not open file %s, %w", u, err)
	}
	return res, nil
}

// Open loads content from git, you can use
// "builtin:///module.star" - load a kube-ci built in
// "file.star" -  from your .kube-ci directory, current ref
// "/file.star" -  from your the root of your repo, current ref
// "https://somehost/file.star" -  (bad idea?
// "github:///file.star" -  from your .kube-ci directory, current ref
// "github://repo/file.star" -  from a repo in your org, absolute from your
// "github://repo.org/file.star?ref=something" -  absolute from your .kube-ci directory, current ref
func parseModURL(base *url.URL, name string) (*url.URL, error) {
	defaultRef := base.Query().Get("ref")

	var defaultOrg string
	if base.Scheme == "github" {
		parts := strings.SplitN(base.Host, ".", 2)
		defaultOrg = parts[1]
	}

	u, err := base.Parse(name)
	if err != nil {
		return nil, err
	}

	if u.Scheme == "github" {
		qs := u.Query()
		if u.Scheme == base.Scheme {
			if len(strings.Split(u.Host, ".")) == 1 {
				u.Host = fmt.Sprintf("%s.%s", u.Host, defaultOrg)
			}

			if qs.Get("ref") == "" {
				qs.Set("ref", defaultRef)
				if u.Host != base.Host {
					qs.Set("ref", "")
				}
				u.RawQuery = qs.Encode()
			}
		} else {
			qs.Set("ref", "")
			u.RawQuery = qs.Encode()
		}
	}

	if err := validateModURL(u); err != nil {
		return nil, err
	}

	return u, nil
}

func (gh *modSrc) openContextFile(thread *starlark.Thread, url *url.URL) (starlark.String, error) {
	if gh.context == nil {
		return emptyStr, fmt.Errorf("file not found in context")
	}

	str, ok := gh.context[url.Path]
	if !ok {
		return emptyStr, fmt.Errorf("file not found in context")
	}
	return starlark.String(str), nil
}

func (gh *modSrc) openContext(thread *starlark.Thread, url *url.URL) (starlark.StringDict, error) {
	str, err := gh.openContextFile(thread, url)
	if err != nil {
		return nil, fmt.Errorf("reading HTTP body failed, %v", err)
	}

	return starlark.ExecFile(thread, url.String(), string(str), gh.predeclared)
}

func (gh *modSrc) openHTTPFile(thread *starlark.Thread, url *url.URL) (starlark.String, error) {
	req, err := http.NewRequest(http.MethodGet, url.String(), nil)
	if err != nil {
		return emptyStr, fmt.Errorf("invalid HTTP request, %v", err)
	}
	resp, err := gh.http.Do(req)
	if err != nil {
		return emptyStr, err
	}
	if resp.StatusCode != http.StatusOK {
		return emptyStr, fmt.Errorf("invalid HTTP status code, %v", resp.StatusCode)
	}

	bs, err := io.ReadAll(resp.Body)
	if err != nil {
		return emptyStr, fmt.Errorf("reading HTTP body failed, %v", err)
	}
	return starlark.String(string(bs)), nil
}

func (gh *modSrc) openHTTP(thread *starlark.Thread, url *url.URL) (starlark.StringDict, error) {
	str, err := gh.openHTTPFile(thread, url)
	if err != nil {
		return nil, fmt.Errorf("reading HTTP body failed, %v", err)
	}

	return starlark.ExecFile(thread, url.String(), string(str), gh.predeclared)
}

func (gh *modSrc) openGithubFile(thread *starlark.Thread, url *url.URL) (starlark.String, error) {
	ctx := context.Background()

	// this has already been validated
	repo, org, _ := strings.Cut(url.Host, ".")
	ref := url.Query().Get("ref")

	file, _, _, err := gh.client.GetContents(
		ctx,
		org,
		repo,
		url.Path,
		&github.RepositoryContentGetOptions{
			Ref: ref,
		},
	)
	if err != nil {
		return emptyStr, err
	}

	str, err := file.GetContent()
	if err != nil {
		return emptyStr, err
	}

	return starlark.String(str), nil
}

func (gh *modSrc) openGithub(thread *starlark.Thread, url *url.URL) (starlark.StringDict, error) {
	str, err := gh.openGithubFile(thread, url)
	if err != nil {
		return nil, err
	}

	mod, err := starlark.ExecFile(thread, url.String(), string(str), gh.predeclared)

	return mod, err
}

func (gh *modSrc) openBuiltin(thread *starlark.Thread, u *url.URL) (starlark.StringDict, error) {
	mod, ok := gh.builtIn[u.Path]
	if !ok {
		return nil, fmt.Errorf("unknown builtin module, %s", u.Path)
	}
	return mod, nil
}

func (gh *modSrc) ModOpen(thread *starlark.Thread, name string) (starlark.StringDict, error) {
	cwu := getCWU(thread)
	u, err := parseModURL(cwu, name)
	if err != nil {
		return nil, err
	}

	var mod starlark.StringDict
	loaders := map[string]func(*starlark.Thread, *url.URL) (starlark.StringDict, error){
		"github":  gh.openGithub,
		"https":   gh.openHTTP,
		"builtin": gh.openBuiltin,
		"context": gh.openContext,
	}

	loader, ok := loaders[u.Scheme]
	if !ok {
		return nil, fmt.Errorf("unsupported scheme, %s", u.Scheme)
	}

	mod, err = loader(thread, u)
	if err != nil {
		return nil, fmt.Errorf("could not open module %s, %w", u, err)
	}
	return mod, nil
}

// modCache is a concurrency-safe, duplicate-suppressing,
// non-blocking modCache of the doLoad function.
// See Section 9.7 of gopl.io for an explanation of this structure.
// It also features online deadlock (load cycle) detection.
type modCache struct {
	modCacheMu sync.Mutex
	modCache   map[string]*entry

	src     modOpener
	starLog func(thread *starlark.Thread, msg string)
}

type entry struct {
	owner   unsafe.Pointer // a *cycleChecker; see cycleCheck
	globals starlark.StringDict
	err     error
	ready   chan struct{}
}

var workingURL = "CWU"

func resolveModuleName(thread *starlark.Thread, module string) *url.URL {
	cwu := getCWU(thread)

	nwu, err := cwu.Parse(module)
	if err != nil {
		thread.Cancel(err.Error())
	}
	if nwu.RawQuery == "" {
		nwu.RawQuery = cwu.RawQuery
	}
	return nwu
}

func (c *modCache) doLoad(_ *starlark.Thread, cc *cycleChecker, modurl *url.URL) (starlark.StringDict, error) {
	thread := &starlark.Thread{
		Name:  "exec " + modurl.String(),
		Print: c.starLog,
		Load: func(thread *starlark.Thread, module string) (starlark.StringDict, error) {
			modURL := resolveModuleName(thread, module)
			return c.get(thread, cc, modURL)
		},
	}

	setCWU(thread, modurl)
	mod, err := c.src.ModOpen(thread, modurl.String())
	if err != nil {
		return nil, err
	}

	return mod, nil
}

// get loads and returns an entry (if not already loaded).
func (c *modCache) get(thread *starlark.Thread, cc *cycleChecker, module *url.URL) (starlark.StringDict, error) {
	moduleName := module.String()

	c.modCacheMu.Lock()
	e := c.modCache[moduleName]
	if e != nil {
		c.modCacheMu.Unlock()
		// Some other goroutine is getting this module.
		// Wait for it to become ready.

		// Detect load cycles to avoid deadlocks.
		if err := cycleCheck(e, cc); err != nil {
			return nil, err
		}

		cc.setWaitsFor(e)
		<-e.ready
		cc.setWaitsFor(nil)
	} else {
		// First request for this module.
		e = &entry{ready: make(chan struct{})}
		c.modCache[moduleName] = e
		c.modCacheMu.Unlock()

		e.setOwner(cc)
		fmt.Println("loading: ", moduleName)
		e.globals, e.err = c.doLoad(thread, cc, module)
		for k, v := range e.globals {
			fmt.Println("symbol: ", k, v.String())
		}
		e.setOwner(nil)

		// Broadcast that the entry is now ready.
		close(e.ready)
	}
	return e.globals, e.err
}

func (c *modCache) Load(thread *starlark.Thread, module string) (starlark.StringDict, error) {
	modURL := resolveModuleName(thread, module)
	return c.get(thread, new(cycleChecker), modURL)
}

// A cycleChecker is used for concurrent deadlock detection.
// Each top-level call to Load creates its own cycleChecker,
// which is passed to all recursive calls it makes.
// It corresponds to a logical thread in the deadlock detection literature.
type cycleChecker struct {
	waitsFor unsafe.Pointer // an *entry; see cycleCheck
}

func (cc *cycleChecker) setWaitsFor(e *entry) {
	atomic.StorePointer(&cc.waitsFor, unsafe.Pointer(e))
}

func (e *entry) setOwner(cc *cycleChecker) {
	atomic.StorePointer(&e.owner, unsafe.Pointer(cc))
}

// cycleCheck reports whether there is a path in the waits-for graph
// from resource 'e' to thread 'me'.
//
// The waits-for graph (WFG) is a bipartite graph whose nodes are
// alternately of type entry and cycleChecker.  Each node has at most
// one outgoing edge.  An entry has an "owner" edge to a cycleChecker
// while it is being readied by that cycleChecker, and a cycleChecker
// has a "waits-for" edge to an entry while it is waiting for that entry
// to become ready.
//
// Before adding a waits-for edge, the modCache checks whether the new edge
// would form a cycle.  If so, this indicates that the load graph is
// cyclic and that the following wait operation would deadlock.
func cycleCheck(e *entry, me *cycleChecker) error {
	for e != nil {
		cc := (*cycleChecker)(atomic.LoadPointer(&e.owner))
		if cc == nil {
			break
		}
		if cc == me {
			return fmt.Errorf("cycle in load graph")
		}
		e = (*entry)(atomic.LoadPointer(&cc.waitsFor))
	}
	return nil
}

func MakeLoad(src modOpener) func(thread *starlark.Thread, module string) (starlark.StringDict, error) {
	modCache := &modCache{
		modCache: make(map[string]*entry),
		src:      src,
	}

	return func(thread *starlark.Thread, module string) (starlark.StringDict, error) {
		return modCache.Load(thread, module)
	}
}

func convertValueToJSON(iv starlark.Value) (interface{}, error) {
	switch v := iv.(type) {
	case *starlark.Dict:
		return convertDictToJSON(v)
	case starlark.String:
		return string(v), nil
	case starlark.Bytes:
		return []byte(v), nil
	case starlark.Bool:
		return bool(v), nil
	case starlark.Int, starlark.Float:
		f, _ := starlark.AsFloat(iv)
		return f, nil
	case *starlark.List:
		return convertListToJSON(v)
	case starlark.NoneType, *starlark.Builtin:
		return nil, nil
	default:
		return nil, fmt.Errorf("could not convert %T to starlark type", iv)
	}
}

func convertListToJSON(l *starlark.List) ([]interface{}, error) {
	res := make([]interface{}, l.Len())
	for i := 0; i < l.Len(); i++ {
		var err error
		res[i], err = convertValueToJSON(l.Index(i))
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

func convertDictToJSON(v *starlark.Dict) (map[string]interface{}, error) {
	res := map[string]interface{}{}
	for _, knv := range v.Keys() {
		kv, ok, err := v.Get(knv)
		if !ok {
			panic("no idea what happened")
		}
		if err != nil {
			panic("no idea what happened")
		}
		jv, err := convertValueToJSON(kv)
		if err != nil {
			panic("no idea what happened")
		}

		switch k := knv.(type) {
		case starlark.String:
			res[string(k)] = jv
		default:
			continue
		}
	}
	return res, nil
}

func ConvertToWorkflow(v starlark.Value) (*workflow.Workflow, error) {
	wvDict, ok := v.(*starlark.Dict)
	if !ok {
		return nil, fmt.Errorf("workflow value is not a dictinoary")
	}

	js, err := convertDictToJSON(wvDict)
	if err != nil {
		return nil, fmt.Errorf("could not convert dictionary to json object, %v", err)
	}

	bs, err := json.Marshal(js)
	if err != nil {
		return nil, fmt.Errorf("could not marshal object to JSON, %v", err)
	}

	decode := scheme.Codecs.UniversalDeserializer().Decode
	obj, _, err := decode(bs, nil, nil)

	if err != nil {
		return nil, fmt.Errorf("could not decode JSON to kubernetes object, %v", err)
	}

	wf, ok := obj.(*workflow.Workflow)
	if !ok {
		return nil, fmt.Errorf("could not use %T as workflow", wf)
	}

	return wf, nil
}

func LoadWorkflow(ctx context.Context, hc *http.Client, fn string, ciContext map[string]string) (*workflow.Workflow, error) {
	yaml, _ := starlibyaml.LoadModule()
	re, _ := starlibre.LoadModule()

	builtins := map[string]starlark.StringDict{
		"/" + starlibyaml.ModuleName: yaml,
		"/" + starlibjson.ModuleName: starlibjson.Module.Members,
		"/" + starlibre.ModuleName:   re,
	}

	src := newModSource(hc)

	predeclared := starlark.StringDict{
		"loadFile": starlark.NewBuiltin("loadFile", src.builtInLoadFile),
	}

	// This dictionary defines the pre-declared environment.
	src.SetPredeclared(predeclared)
	src.SetBuiltIn(builtins)
	src.SetContext(ciContext)

	loader := MakeLoad(src)

	// The Thread defines the behavior of the built-in 'print' function.
	thread := &starlark.Thread{
		Name: "main",
		//Print: func(_ *starlark.Thread, msg string) { t.Logf(msg) },
		Load: loader,
	}

	u, _ := url.Parse(fmt.Sprintf("context:///%s", fn))
	setCWU(thread, u)

	script, ok := ciContext[fn]
	if !ok {
		return nil, fmt.Errorf("file %s is not present in the CI context", fn)
	}

	val, err := starlark.ExecFile(thread, "main", script, src.predeclared)
	if err != nil {
		return nil, err
	}

	if !val.Has("workflow") {
		return nil, fmt.Errorf("starlark result must contain 'workflow', %v", err)
	}
	wv := val["workflow"]

	wf, err := ConvertToWorkflow(wv)
	if err != nil {
		return nil, fmt.Errorf("starlark result could not be marshalled to a workflow, %v", err)
	}

	return wf, nil
}
