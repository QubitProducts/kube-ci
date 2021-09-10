package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"

	pb "github.com/QubitProducts/kube-ci/pb"

	"github.com/improbable-eng/grpc-web/go/grpcweb"
	grpc "google.golang.org/grpc"
)

//go:embed ui/build
var staticFiles embed.FS

func getFileSystem() http.FileSystem {
	fsys, err := fs.Sub(staticFiles, "ui/build")
	if err != nil {
		log.Fatal(err)
	}

	return http.FS(fsys)
}

func UIServer(addr string, uiSrvr pb.KubeCIServer) {
	var opts []grpc.ServerOption

	grpcServer := grpc.NewServer(opts...)
	pb.RegisterKubeCIServer(grpcServer, uiSrvr)

	wrappedGrpc := grpcweb.WrapServer(grpcServer)

	fs := http.FileServer(getFileSystem())

	mux := http.NewServeMux()
	// Serve static files
	mux.Handle("/", fs)
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/"
		fs.ServeHTTP(w, r)
	})
	mux.HandleFunc("/job/", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/"
		fs.ServeHTTP(w, r)
	})
	mux.HandleFunc("/profiles", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/"
		fs.ServeHTTP(w, r)
	})
	mux.HandleFunc("/archive", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/"
		fs.ServeHTTP(w, r)
	})
	mux.HandleFunc("/archive/", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/"
		fs.ServeHTTP(w, r)
	})

	h := http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		if wrappedGrpc.IsGrpcWebRequest(req) {
			wrappedGrpc.ServeHTTP(resp, req)
			return
		}
		// Fall back to other servers.
		mux.ServeHTTP(resp, req)
	})

	http.ListenAndServe(addr, h)
}
