# syntax=docker/dockerfile:1
# The reasonr-builder image: reasonr-build with the Go toolchain, so that a
# binary is built from a manifest without a local Go (plan 0022, decision 7):
#
#   docker run --rm -v "$PWD:/work" reasonr-builder:0.1.0 -f components.yaml -o reasonrd
#
# The build context is the repository root (make images):
#
#   docker build -f cmd/reasonr-build/Dockerfile -t reasonr-builder:0.1.0 .
FROM golang:1.23 AS build
WORKDIR /src
COPY . ./
RUN CGO_ENABLED=0 go build -o /reasonr-build ./cmd/reasonr-build

FROM golang:1.23
COPY --from=build /reasonr-build /usr/local/bin/reasonr-build
WORKDIR /work
ENTRYPOINT ["reasonr-build"]
