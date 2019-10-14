FROM golang:1.12-alpine3.9 as builder

WORKDIR /go/src/gitlab.tools.in.pan-net.eu/security/kafka-go-streamer
ENV GOPATH=/go

RUN set -ex \
    && apk add --no-cache --virtual .build-deps \
            git \
            gcc \
            g++ \
            make \
            libc-dev \
            musl-dev \
            linux-headers \
            curl
RUN curl https://raw.githubusercontent.com/golang/dep/master/install.sh | sh
COPY . /go/src/gitlab.tools.in.pan-net.eu/security/kafka-go-streamer

RUN dep ensure
RUN go build

FROM gitlab.tools.in.pan-net.eu:4567/docker-images/pannet-alpine:v0.1.4-3.9

COPY --from=builder /go/src/gitlab.tools.in.pan-net.eu/security/kafka-go-streamer/kafka-go-streamer /bin/kafka-go-streamer

USER nobody

ENTRYPOINT ["/bin/kafka-go-streamer"]
