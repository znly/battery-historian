#!/bin/bash

go build cmd/battery-historian/battery-historian.go
cp battery-historian /output
go run setup.go
