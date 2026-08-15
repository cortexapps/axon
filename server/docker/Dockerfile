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

EXPOSE 50052 8080

ENTRYPOINT ["/usr/local/bin/axon-tunnel-server"]
