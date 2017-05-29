FROM alpine:latest
MAINTAINER Trickle Cloud <github@trickle.cloud>

RUN apk --no-cache add \
    curl \
    ffmpeg \
    wget \
    x264

WORKDIR /data

COPY trickle-linux-amd64 /usr/bin/trickle

ENTRYPOINT ["/usr/bin/trickle"]
