#!/bin/bash

go build cmd/battery-historian/battery-historian.go
cp battery-historian compiled/
go run setup.go
