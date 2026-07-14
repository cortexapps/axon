# Stage 1: Build the Go agent binary
FROM debian:stable-slim AS builder

RUN apt-get update && apt-get upgrade -y && apt-get install -y protobuf-compiler git wget build-essential

ENV GOLANG_VERSION=1.26.5
RUN wget -q "https://dl.google.com/go/go${GOLANG_VERSION}.linux-$(dpkg --print-architecture).tar.gz" -O /tmp/go.tar.gz \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm /tmp/go.tar.gz

ENV GOROOT="/usr/local/go"
ENV GOPATH="/root/go"
ENV GOPROXY="https://proxy.golang.org,direct"
ENV PATH="${GOROOT}/bin:${GOPATH}/bin:${PATH}"

COPY agent/. /build/.
RUN make -C /build setup proto
RUN cd /build && go build -o /agent/cortex-axon-agent

# Stage 2: Runtime image
FROM debian:stable-slim
WORKDIR /agent

# APT_CACHE_BUST participates in buildx's layer cache key for the RUN below.
# Bump its value (a one-line PR is enough) to invalidate the cached layer and
# force `apt-get update && upgrade` to re-fetch from Debian's archive. Use this
# when the scheduled Trivy scan flags OS-package CVEs whose fixes are already
# in the archive — the cache is just serving a stale layer. Leaves the rest of
# the build (Go, npm, snyk-broker clone) hitting cache as normal.
ARG APT_CACHE_BUST=2026-07-14
RUN echo "apt cache bust: $APT_CACHE_BUST" \
    && apt-get update && apt-get upgrade -y && apt-get install -y \
    protobuf-compiler git python3 python3-venv wget build-essential openssl jq

# Install NodeJS and Snyk Broker
ENV NODE_VERSION=22

ARG SNYK_BROKER_VERSION=v1.0.16-axon
RUN wget -q -O - https://deb.nodesource.com/setup_${NODE_VERSION}.x | bash - && apt-get install -y nodejs
RUN npm install --global npm@11.18.0 typescript@4.9.3
RUN git clone https://github.com/cortexapps/snyk-broker.git /tmp/snyk-broker && \
    cd /tmp/snyk-broker && \
    git checkout ${SNYK_BROKER_VERSION} && \
    npm install && \
    cp -r . /usr/local/snyk-broker && \
    cd /usr/local/snyk-broker && \
    npm install --global . && \
    npm prune --omit=dev && \
    rm -rf /tmp/snyk-broker

# Add npm global bin to PATH
ENV PATH="/usr/local/lib/node_modules/.bin:${PATH}"

# Install Go (needed by scaffold apps that build on top of this image)
ENV GOLANG_VERSION=1.26.5
RUN wget -q "https://dl.google.com/go/go${GOLANG_VERSION}.linux-$(dpkg --print-architecture).tar.gz" -O /tmp/go.tar.gz \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm /tmp/go.tar.gz

ENV GOROOT="/usr/local/go"
ENV GOPATH="/root/go"
ENV GOPROXY="https://proxy.golang.org,direct"
ENV PATH="${GOROOT}/bin:${GOPATH}/bin:${PATH}:/agent"

# Copy the built binary from the builder stage
COPY --from=builder /agent/cortex-axon-agent /agent/cortex-axon-agent

COPY agent/server/snykbroker/accept_files /agent/accept_files
ENV ACCEPTFILE_DIR=/agent/accept_files

COPY agent/server/snykbroker/plugins /agent/plugins
ENV BUILTIN_PLUGIN_DIR=/agent/plugins

COPY /scaffold/. /app/scaffold/.

COPY docker/start /agent/start
COPY docker/entrypoint.sh /agent/entrypoint.sh
COPY docker/app_entrypoint.sh /app/app_entrypoint.sh
COPY docker/scripts /agent/scripts

ARG BUILD_VERSION=dev
ENV AXON_BUILD_VERSION=${BUILD_VERSION}
ENV CORTEX_API_BASE_URL=https://api.getcortexapp.com
ENV HANDLER_HISTORY_PATH=/var/log/axon/history
ENV NODE_OPTIONS=--use-openssl-ca

EXPOSE 50051

ENTRYPOINT [ "/agent/entrypoint.sh" ]
CMD [ "serve" ]
