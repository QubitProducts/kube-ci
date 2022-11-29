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

package kubeci

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
)

var ()

func rootHandler(w http.ResponseWriter, _ *http.Request) {
	resp := map[string]string{
		"hello": "world",
	}

	bs, _ := json.Marshal(resp)
	buf := bytes.NewBuffer(bs)

	if _, err := io.Copy(w, buf); err != nil {
		panic(err)
	}
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

func DefaultMain() {
	if err := argoScheme.AddToScheme(scheme.Scheme); err != nil {
		panic(err)
	}

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

	keybs, err := os.ReadFile(*keyfile)
	if err != nil {
		log.Fatalf("failed to read keys files, %v", err)
	}

	var ids []int
	if len(*idsfile) > 0 {
		var file *os.File
		file, err = os.Open(*idsfile)
		if err != nil {
			log.Fatalf("failed to open ids files, %v", err)
		}
		lr := bufio.NewReader(file)
		for {
			var line []byte
			line, _, err = lr.ReadLine()
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
			var id int
			id, err = strconv.Atoi(idStr)
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

	secret, err := os.ReadFile(*secretfile)
	if err != nil {
		log.Fatalf("failed to read secrets file, %v", err)
	}
	secret = bytes.TrimSpace(secret)

	wfconfig, err := ReadConfig(*configfile, namespace)
	if err != nil {
		log.Fatalf("failed to read config file, %v", err)
	}

	ghSrc := &GithubKeyStore{
		BaseTransport: http.DefaultTransport,
		AppID:         *appID,
		Key:           bytes.TrimSpace(keybs),
		IDs:           ids,
		Orgs:          orgsrx,
	}

	storage := &K8sStorageManager{
		Namespace:  wfconfig.Namespace,
		KubeClient: kubeClient,
		ManagedBy:  wfconfig.ManagedBy,
	}

	wfSyncer := NewWorkflowSyner(
		kubeClient,
		wfClient,
		sinf,
		storage,
		ghSrc,
		*appID,
		secret,
		*argoUIBaseURL,
		*wfconfig,
	)

	shutdownInt := int32(200)
	sh := &StatusHandler{
		Status: &shutdownInt,
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
		slack, err := NewSlack(*slackTokenFile, *slackSigningSecretFile, wfconfig.CIContextPath, wfconfig.TemplateSet)
		if err != nil {
			log.Fatalf("couldn't setup slack, %v", err)
		}
		mux.Handle("/webhooks/slack/cmd", slack)
	}

	slashHandler := &SlashHandler{
		CIContextPath:  wfconfig.CIContextPath,
		CIYAMLFile:     wfconfig.CIYAMLFile,
		CIStarlarkFile: wfconfig.CIStarlarkFile,
		Templates:      wfconfig.TemplateSet,
		Runner:         wfSyncer,
	}

	hookHandler := &HookHandler{
		Slash:   slashHandler,
		Runner:  wfSyncer,
		Clients: ghSrc,
		Storage: storage,

		UIBase:       *argoUIBaseURL,
		AppID:        *appID,
		GitHubSecret: secret,
	}

	apiHandler := NewAPIHandler(APIHandler{
		Storage: storage,
		Clients: ghSrc,
		Runner:  wfSyncer,
		Slash:   slashHandler,

		UIBase:       *argoUIBaseURL,
		GitHubSecret: secret,
		AppID:        *appID,
		InstallID:    wfconfig.API.DefaultInstallID,
	})

	mux.HandleFunc("/webhooks/github",
		promhttp.InstrumentHandlerDuration(
			duration,
			httpErrorFunc(hookHandler.LoggingWebhook)))

	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/status", sh)
	mux.Handle("/api/v1/", http.StripPrefix("/api/v1", apiHandler))

	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  15 * time.Second,
	}

	sctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)
	signal.Notify(quit, syscall.SIGTERM)

	go func() {
		sinf.Start(sctx.Done())
		go func() {
			if err := wfSyncer.Run(sctx.Done()); err != nil {
				log.Printf("error running workflow syncer, %v", err)
			}
		}()

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

		cancel()
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Could not listen on %s: %v\n", ":8080", err)
	}
}
