FROM golang:1.9
MAINTAINER viewscreen Cloud <github@viewscreen.cloud>

RUN apt-get update && apt-get install -y git && rm -rf /var/lib/apt/lists/*

WORKDIR /go/src/github.com/viewscreen/viewscreen

RUN go get \
    github.com/jteeuwen/go-bindata/... \
    github.com/PuerkitoBio/goquery \
    github.com/armon/circbuf \
    github.com/disintegration/imaging \
    github.com/dustin/go-humanize \
    github.com/julienschmidt/httprouter \
    github.com/eduncan911/podcast \
    github.com/rylio/ytdl \
    go.uber.org/zap \
    golang.org/x/crypto/acme/autocert \
    github.com/anacrolix/torrent \
    github.com/anacrolix/utp \
    golang.org/x/time/rate


COPY *.go ./
COPY internal ./internal
COPY static ./static
COPY templates ./templates

ARG BUILD_VERSION=unknown

ENV GODEBUG="netdns=go http2server=0"
ENV GOPATH="/go"


RUN go fmt && \
    go vet --all && \
    go-bindata --pkg main static/... templates/...

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -v --compiler gc --ldflags "-extldflags -static -s -w -X main.version=${BUILD_VERSION}" -o /usr/bin/viewscreen-linux-amd64

RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
    go build -v --compiler gc --ldflags "-extldflags -static -s -w -X main.version=${BUILD_VERSION}" -o /usr/bin/viewscreen-linux-armv7

RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build -v --compiler gc --ldflags "-extldflags -static -s -w -X main.version=${BUILD_VERSION}" -o /usr/bin/viewscreen-linux-arm64

