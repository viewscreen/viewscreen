FROM alpine:latest
MAINTAINER Watcher Cloud <github@watcher.cloud>

RUN apk --no-cache add \
    curl \
    ffmpeg \
    wget \
    x264

WORKDIR /data

COPY watcher-linux-amd64 /usr/bin/watcher

ENTRYPOINT ["/usr/bin/watcher"]
