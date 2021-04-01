module github.com/QubitProducts/kube-ci

require (
	github.com/argoproj/argo v0.0.0-20201019203908-5eebce9af440 // v2.11.6
	github.com/bradleyfalzon/ghinstallation v1.1.1
	github.com/golang/protobuf v1.4.3
	github.com/google/go-github/v29 v29.0.3 // indirect
	github.com/google/go-github/v32 v32.1.0
	github.com/google/uuid v1.1.2 // indirect
	github.com/googleapis/gnostic v0.3.1 // indirect
	github.com/mattn/go-shellwords v1.0.5
	github.com/pkg/errors v0.9.1
	github.com/prometheus/client_golang v1.0.0
	github.com/prometheus/client_golang v1.5.1-0.20210129212427-08104a0ef98e
	github.com/slack-go/slack v0.8.2
	github.com/slack-go/slack v0.8.2
	golang.org/x/crypto v0.0.0-20200820211705-5c72a883971a // indirect
	golang.org/x/crypto v0.0.0-20200820211705-5c72a883971a // indirect
	golang.org/x/mod v0.3.1-0.20200828183125-ce943fd02449 // indirect
	golang.org/x/mod v0.3.1-0.20200828183125-ce943fd02449 // indirect
	golang.org/x/net v0.0.0-20210226172049-e18ecbb05110 // indirect
	golang.org/x/sys v0.0.0-20210305230114-8fe3ee5dd75b // indirect
	golang.org/x/text v0.3.5 // indirect
	google.golang.org/genproto v0.0.0-20210303154014-9728d6b83eeb // indirect
	google.golang.org/grpc v1.36.0
	google.golang.org/grpc/cmd/protoc-gen-go-grpc v1.1.0
	google.golang.org/protobuf v1.25.0
	gopkg.in/yaml.v2 v2.2.8
	honnef.co/go/tools v0.0.1-2020.1.6
	k8s.io/api v0.17.8
	k8s.io/apimachinery v0.17.8
	k8s.io/client-go v0.17.8
)

go 1.13

replace (
	github.com/grpc-ecosystem/grpc-gateway => github.com/grpc-ecosystem/grpc-gateway v1.12.2
	sigs.k8s.io/controller-tools => sigs.k8s.io/controller-tools v0.2.9
)
