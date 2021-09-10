package kubeci

//go:generate protoc -I. --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative     kubeci.proto
//go:generate protoc -I. --plugin=protoc-gen-ts=../ui/node_modules/.bin/protoc-gen-ts --js_out=import_style=commonjs,binary:../ui/src --ts_out=service=grpc-web:../ui/src kubeci.proto
