FROM golang:1.15-alpine as builder

WORKDIR /go/src/gitlab.tools.in.pan-net.eu/security/kafka-go-streamer
ENV GOPATH=/go
ENV GO111MODULE=on

RUN set -ex \
    && apk add --no-cache --virtual .build-deps \
            git \
            gcc \
            g++ \
            make \
            libc-dev \
            musl-dev \
            linux-headers

COPY . /go/src/gitlab.tools.in.pan-net.eu/security/kafka-go-streamer

RUN go build

FROM gitlab.tools.in.pan-net.eu:4567/docker-images/pannet-alpine:v0.1.4-3.9

COPY --from=builder /go/src/gitlab.tools.in.pan-net.eu/security/kafka-go-streamer/kafka-go-streamer /bin/kafka-go-streamer

USER nobody

ENTRYPOINT ["/bin/kafka-go-streamer"]
