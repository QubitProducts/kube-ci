// Copyright 2019 Qubit Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	clientset "github.com/argoproj/argo-workflows/v3/pkg/client/clientset/versioned"
	argoScheme "github.com/argoproj/argo-workflows/v3/pkg/client/clientset/versioned/scheme"
	informers "github.com/argoproj/argo-workflows/v3/pkg/client/informers/externalversions"
	"gopkg.in/yaml.v2"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
)

var ()

func rootHandler(w http.ResponseWriter, r *http.Request) {
	resp := map[string]string{
		"hello": "world",
	}

	bs, _ := json.Marshal(resp)
	buf := bytes.NewBuffer(bs)

	io.Copy(w, buf)
}

type httpErrorFunc func(w http.ResponseWriter, r *http.Request) (int, string)

func (f httpErrorFunc) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	code, status := f(w, r)

	if code == 0 {
		// We'll use 0 to indicate that the request output
		// as already been handled.
		return
	}

	if code == 0 {
		code = http.StatusBadRequest
	}

	if status == "" {
		status = http.StatusText(code)
	}

	http.Error(w, status, code)
}

func main() {
	argoScheme.AddToScheme(scheme.Scheme)

	kubeconfig := flag.String("kubeconfig", "", "Path to a kubeconfig file")
	masterURL := flag.String("master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	shutdownGrace := flag.Duration("grace.shutdown", 2*time.Second, "delay before server shuts down")
	requestGrace := flag.Duration("grace.requests", 1*time.Second, "delay before server starts shut down")
	configfile := flag.String("config", "", "configuration options")
	keyfile := flag.String("keyfile", "github-key", "github application key")
	secretfile := flag.String("secretfile", "webhook-secret", "file containing your webhook secret")
	appID := flag.Int64("github.appid", 0, "github application ID")
	idsfile := flag.String("idsfile", "", "file containing newline delimited list of install-ids to accept events from, if not provided, or empty, all intall-ids are accepted")

	orgs := flag.String("orgs", "", "regex of orgs to accept events from, start and end anchors will be added")
	argoUIBaseURL := flag.String("argo.ui.base", "http://argo", "file containing your webhook secret")

	slackTokenFile := flag.String("slack.tokenfile", "", "slack token store in this file")
	slackSigningSecretFile := flag.String("slack.secretfile", "", "slack signing secret")

	flag.Parse()

	if *appID == 0 {
		log.Fatal("github appID must be set")
	}

	config, err := clientcmd.BuildConfigFromFlags(*masterURL, *kubeconfig)
	if err != nil {
		log.Fatalf("Error building kubeconfig: %s", err.Error())
	}

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Error building kubernetes clientset: %s", err.Error())
	}

	wfClient, err := clientset.NewForConfig(config)
	if err != nil {
		log.Fatalf("failed to create argo client, %v", err)
	}

	namespace := "argo"
	selector := "managedBy=kube-ci"

	sinf := informers.NewFilteredSharedInformerFactory(
		wfClient,
		0,
		namespace,
		func(opt *metav1.ListOptions) {
			opt.LabelSelector = selector
		},
	)

	keybs, err := ioutil.ReadFile(*keyfile)
	if err != nil {
		log.Fatalf("failed to read keys files, %v", err)
	}

	var ids []int
	if len(*idsfile) > 0 {
		file, err := os.Open(*idsfile)
		if err != nil {
			log.Fatalf("failed to open ids files, %v", err)
		}
		lr := bufio.NewReader(file)
		for {
			line, _, err := lr.ReadLine()
			if err == io.EOF {
				break
			}
			if err != nil {
				log.Fatalf("failed to read id file, %v", err)
			}
			idStr := strings.TrimSpace(string(line))
			if idStr == "" {
				continue
			}
			id, err := strconv.Atoi(idStr)
			if err != nil {
				log.Fatalf("failed to parse org id %q as integer, %v", idStr, err)
			}

			ids = append(ids, id)
		}
	}

	var orgsrx *regexp.Regexp
	if len(*orgs) > 0 {
		rxstr := *orgs
		if !strings.HasPrefix(rxstr, "^") {
			rxstr = "^" + rxstr
		}
		if !strings.HasSuffix(rxstr, "$") {
			rxstr = rxstr + "$"
		}
		orgsrx, err = regexp.Compile(rxstr)
		if err != nil {
			log.Fatalf("failed to parse orgs regexp, %v", err)
		}
	}

	secret, err := ioutil.ReadFile(*secretfile)
	if err != nil {
		log.Fatalf("failed to read secrets file, %v", err)
	}
	secret = bytes.TrimSpace(secret)

	wfconfig := Config{
		CIFilePath:    ".kube-ci/ci.yaml",
		Namespace:     namespace,
		BuildDraftPRs: false,
		BuildBranches: "master",
	}

	if *configfile != "" {
		bs, err := ioutil.ReadFile(*configfile)
		if err != nil {
			log.Fatalf("failed to read config file, %v", err)
		}
		err = yaml.Unmarshal(bs, &wfconfig)
		if err != nil {
			log.Fatalf("failed to parse config file, %v", err)
		}
	}

	wfconfig.buildBranches, err = regexp.Compile(wfconfig.BuildBranches)
	if err != nil {
		log.Fatalf("failed to compile branches regexp, %v", err)
	}

	wfconfig.deployTemplates, err = regexp.Compile(wfconfig.DeployTemplates)
	if err != nil {
		log.Fatalf("failed to compile deployTemplates regexp, %v", err)
	}

	ghSrc := &githubKeyStore{
		baseTransport: http.DefaultTransport,
		appID:         *appID,
		key:           bytes.TrimSpace(keybs),
		ids:           ids,
		orgs:          orgsrx,
	}

	storage := &k8sStorageManager{
		namespace:  wfconfig.Namespace,
		kubeclient: kubeClient,
	}

	wfSyncer := newWorkflowSyncer(
		kubeClient,
		wfClient,
		sinf,
		storage,
		ghSrc,
		*appID,
		secret,
		*argoUIBaseURL,
		wfconfig,
	)

	shutdownInt := int32(200)
	sh := &statusHandler{
		shutdown: &shutdownInt,
	}

	duration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "A histogram of latencies for requests.",
			Buckets: []float64{.001, .005, 0.010, 0.05, 0.5, 1.0},
		},
		[]string{},
	)
	prometheus.MustRegister(duration)

	mux := http.NewServeMux()
	mux.HandleFunc("/",
		promhttp.InstrumentHandlerDuration(
			duration,
			http.HandlerFunc(rootHandler)))

	if *slackTokenFile != "" {
		slack, err := newSlack(*slackTokenFile, *slackSigningSecretFile, wfconfig.CIFilePath, wfconfig.TemplateSet)
		if err != nil {
			log.Fatalf("couldn't setup slack, %v", err)
		}
		mux.Handle("/webhooks/slack/cmd", slack)
	}

	slashHandler := &slashHandler{
		ciFilePath: wfconfig.CIFilePath,
		templates:  wfconfig.TemplateSet,
		runner:     wfSyncer,
	}

	hookHandler := &hookHandler{
		slash:   slashHandler,
		runner:  wfSyncer,
		clients: ghSrc,
		storage: storage,

		uiBase:   *argoUIBaseURL,
		appID:    *appID,
		ghSecret: secret,
	}

	mux.HandleFunc("/webhooks/github",
		promhttp.InstrumentHandlerDuration(
			duration,
			httpErrorFunc(hookHandler.loggingWebhook)))

	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/status", sh)

	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  15 * time.Second,
	}

	done := make(chan struct{})
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)
	signal.Notify(quit, syscall.SIGTERM)

	go func() {
		sinf.Start(done)
		go wfSyncer.Run(done)

		<-quit

		log.Println("setting status to shutdown")
		sh.Shutdown()

		log.Printf("sleeping for %s before shutdown", shutdownGrace)
		time.Sleep(*shutdownGrace)

		ctx, cancel := context.WithTimeout(context.Background(), *requestGrace)
		defer cancel()

		log.Printf("shutting down the server")
		server.SetKeepAlivesEnabled(false)
		if err := server.Shutdown(ctx); err != nil {
			log.Fatalf("Could not gracefully shutdown the server: %v\n", err)
		}
		log.Printf("server shutdown")

		close(done)
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Could not listen on %s: %v\n", ":8080", err)
	}
}
