FROM alpine:latest
MAINTAINER viewscreen <viewscreen@portal.cloud>

RUN apk --no-cache add \
    curl \
    ffmpeg \
    wget \
    x264

WORKDIR /data

COPY viewscreen-linux-amd64 /usr/bin/viewscreen

ENTRYPOINT ["/usr/bin/viewscreen"]
