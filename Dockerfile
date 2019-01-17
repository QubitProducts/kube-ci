# build stage
FROM golang:1.11 AS build-env
ADD . /src
RUN cd /src &&  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o goapp .

# final stage
FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=build-env /src/goapp /app/
ENTRYPOINT ["/app/goapp"]
