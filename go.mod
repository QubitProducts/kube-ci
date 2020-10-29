module github.com/QubitProducts/kube-ci

require (
	github.com/18F/hmacauth v0.0.0-20151013130326-9232a6386b73
	github.com/argoproj/argo v0.0.0-20201019203908-5eebce9af440 // v2.11.6
	github.com/argoproj/pkg v0.2.0 // indirect
	github.com/blushft/go-diagrams v0.0.0-20201006005127-c78c821223d9 // indirect
	github.com/bmizerany/assert v0.0.0-20160611221934-b7ed37b82869
	github.com/bradleyfalzon/ghinstallation v1.1.1
	github.com/google/go-github/v29 v29.0.3 // indirect
	github.com/google/go-github/v32 v32.1.0
	github.com/googleapis/gnostic v0.3.1 // indirect
	github.com/gorilla/websocket v1.4.2
	github.com/mattn/go-shellwords v1.0.5
	github.com/mreiferson/go-options v1.0.0 // indirect
	github.com/opentracing-contrib/go-stdlib v0.0.0-20190519235532-cf7a6c988dc9
	github.com/opentracing/opentracing-go v1.1.0
	github.com/pkg/errors v0.9.1
	github.com/prometheus/client_golang v1.0.0
	github.com/pusher/oauth2_proxy v1.1.2-0.20200308153134-e3fb25efe6c5
	golang.org/x/crypto v0.0.0-20200820211705-5c72a883971a // indirect
	gopkg.in/yaml.v2 v2.2.8
	honnef.co/go/tools v0.0.1-2020.1.6 // indirect
	k8s.io/api v0.17.8
	k8s.io/apimachinery v0.17.8
	k8s.io/client-go v0.17.8
)

go 1.13

replace (
	github.com/grpc-ecosystem/grpc-gateway => github.com/grpc-ecosystem/grpc-gateway v1.12.2
	sigs.k8s.io/controller-tools => sigs.k8s.io/controller-tools v0.2.9
)
