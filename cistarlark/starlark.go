// Copyright 2017 The Bazel Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cistarlark

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/google/go-github/v45/github"
	"go.starlark.net/starlark"
)

type githubContentGetter interface {
	DownloadContents(ctx context.Context, org, repo, filepath string, opts *github.RepositoryContentGetOptions) (io.ReadCloser, error)
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

	client githubContentGetter
	http   httpDoer
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
			return fmt.Errorf("host is not invalid for builtin")
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
		"github": gh.openGithubFile,
		"https":  gh.openHTTPFile,
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

	reader, err := gh.client.DownloadContents(
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

	buf := bytes.NewBuffer(nil)
	io.Copy(buf, reader)

	return starlark.String(buf.String()), nil
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
