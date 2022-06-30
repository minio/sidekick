FROM golang:1.18

ADD go.mod /go/src/github.com/minio/sidekick/go.mod
ADD go.sum /go/src/github.com/minio/sidekick/go.sum
WORKDIR /go/src/github.com/minio/sidekick/
# Get dependencies - will also be cached if we won't change mod/sum
RUN go mod download

ADD . /go/src/github.com/minio/sidekick/
WORKDIR /go/src/github.com/minio/sidekick/

ENV CGO_ENABLED=0

RUN go build -ldflags '-w -s' -a -o sidekick .

FROM scratch
MAINTAINER MinIO Development "dev@min.io"
EXPOSE 8080

COPY --from=0 /go/src/github.com/minio/sidekick/sidekick /sidekick

ENTRYPOINT ["/sidekick"]
