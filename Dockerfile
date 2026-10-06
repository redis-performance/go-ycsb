FROM golang:1.25-alpine3.21

ENV GOPATH /go

RUN apk update && apk upgrade && \
    apk add --no-cache git build-base

RUN mkdir -p /go/src/github.com/pingcap/go-ycsb
WORKDIR /go/src/github.com/pingcap/go-ycsb

COPY go.mod .
COPY go.sum .

RUN GO111MODULE=on go mod download

COPY . .

ARG VERSION=unknown
RUN GO111MODULE=on go build -ldflags "-X github.com/pingcap/go-ycsb/pkg/measurement.Version=${VERSION}" -o /go-ycsb ./cmd/*

FROM alpine:3.21

RUN apk add --no-cache dumb-init

COPY --from=0 /go-ycsb /go-ycsb

ADD workloads /workloads

EXPOSE 6060

ENTRYPOINT [ "/usr/bin/dumb-init", "/go-ycsb" ]
