FROM golang:alpine AS builder
RUN apk --update --no-cache add git
COPY . /go/src/github.com/zachomedia/k8s-https-check
WORKDIR /go/src/github.com/zachomedia/k8s-https-check
ENV GOOS linux
ENV GOARCH amd64
ENV CGO_ENABLED 0
RUN go get && go build

FROM alpine:latest
COPY --from=builder /go/src/github.com/zachomedia/k8s-https-check k8s-https-check
ENTRYPOINT [ "/k8s-https-check" ]
