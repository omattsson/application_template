# syntax=docker/dockerfile:1
# The build stage runs on the build machine's platform ($BUILDPLATFORM) and
# cross-compiles for $TARGETOS/$TARGETARCH. A multi-platform build therefore
# never runs the Go compiler under QEMU emulation.

# For development mode (hot reload with air)
FROM golang:1.27.3 AS development
WORKDIR /app
RUN go install github.com/cosmtrek/air@v1.49.0
CMD ["air"]

# For production mode — build stage. The module download does not depend on
# the target platform, so one cached layer serves all platforms. The modules
# stay in the image layer (not in a cache mount): the GitHub Actions cache
# stores layers, but not cache mounts.
FROM --platform=$BUILDPLATFORM golang:1.27.3 AS build-prod
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o main ./api/

# Final production image: minimal Alpine runtime, non-root user, binary only.
FROM alpine:3.24 AS production
# The upgrade picks up fixed Alpine packages that are newer than the
# alpine:3.24 base image.
RUN apk upgrade --no-cache && \
    apk add --no-cache ca-certificates
WORKDIR /app/
# The binary lives outside /app so that a development bind mount of the
# source tree over /app does not hide it.
COPY --from=build-prod /app/main /usr/local/bin/server
EXPOSE 8081
RUN adduser -D -u 65532 nonroot
USER 65532:65532
CMD ["server"]
