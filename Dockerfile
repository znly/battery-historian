FROM golang:1.7

ENV GO_PACKAGE github.com/google/battery-historian

COPY . /go/src/${GO_PACKAGE}

WORKDIR /go/src/${GO_PACKAGE}

ENTRYPOINT "./compiled/battery-historian"

EXPOSE 9999
