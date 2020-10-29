# build stage
FROM golang:1.14 AS build-env
ADD . /src
WORKDIR /src
RUN GOOS=linux GOARCH=amd64 go test -v -race .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o goapp .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o oauth2_proxy  github.com/pusher/oauth2_proxy


# final stage
FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=build-env /src/goapp /app/
COPY --from=build-env /src/oauth2_proxy /app/
ENTRYPOINT ["/app/goapp"]
