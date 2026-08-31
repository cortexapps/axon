FROM golang:1.25-bookworm AS builder

RUN apt-get update && apt-get install -y protobuf-compiler

WORKDIR /build

# Copy proto definitions.
COPY proto/ /proto/

# Copy server source and generate proto.
COPY server/ /build/
RUN make setup proto
RUN go build -o /axon-tunnel-server ./cmd/...

# Runtime image.
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*

COPY --from=builder /axon-tunnel-server /usr/local/bin/axon-tunnel-server

# Running in a container means running as a deployed service, so log as one:
# ENV=production
ENV ENV=production

# Stamps the build this image was made from, the same contract the agent image
# uses (docker/Dockerfile). CI passes <ref>-<sha>; an unstamped build is "dev".
# The server logs it at startup and serves it from /healthcheck, so a running
# pod can be matched to a release without trusting its image tag.
ARG BUILD_VERSION=dev
ENV AXON_BUILD_VERSION=${BUILD_VERSION}

EXPOSE 50052 8080

ENTRYPOINT ["/usr/local/bin/axon-tunnel-server"]
