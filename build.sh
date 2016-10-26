#!/bin/bash
docker build --file Dockerfile.compile . -t battery-historian:compile
docker run -v $(pwd)/bin:/output -v $(pwd)/compiled:/go/src/github.com/google/battery-historian/compiled battery-historian:compile
docker build . -t battery-historian:latest
