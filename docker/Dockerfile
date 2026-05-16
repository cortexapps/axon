# Stage 1: Build the Go agent binary
FROM debian:stable-slim AS builder

RUN apt-get update && apt-get upgrade -y && apt-get install -y protobuf-compiler git wget build-essential

ENV GOLANG_VERSION=1.26.3
RUN wget -q "https://dl.google.com/go/go${GOLANG_VERSION}.linux-$(dpkg --print-architecture).tar.gz" -O /tmp/go.tar.gz \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm /tmp/go.tar.gz

ENV GOROOT="/usr/local/go"
ENV GOPATH="/root/go"
ENV GOPROXY="https://proxy.golang.org,direct"
ENV PATH="${GOROOT}/bin:${GOPATH}/bin:${PATH}"

COPY agent/. /build/.
COPY proto/ /proto/
RUN make -C /build setup proto
RUN cd /build && go build -o /agent/cortex-axon-agent

# Stage 2: Runtime image
FROM debian:stable-slim
WORKDIR /agent

# Install dependencies. libngtcp2-16, libngtcp2-crypto-gnutls8, and libnghttp2-14
# are pinned to patched Debian versions to address CVE-2026-40170 and
# CVE-2026-27135; they're pulled transitively via wget -> libcurl3-gnutls. Bump
# these pins if Debian ships a newer fix or the current version ages out of the
# archive.
RUN apt-get update && apt-get upgrade -y && apt-get install -y \
    protobuf-compiler git python3 python3-venv wget build-essential openssl jq \
    libngtcp2-16=1.11.0-1+deb13u1 \
    libngtcp2-crypto-gnutls8=1.11.0-1+deb13u1 \
    libnghttp2-14=1.64.0-1.1+deb13u1

# Install NodeJS and Snyk Broker
ENV NODE_VERSION=20

ARG SNYK_BROKER_VERSION=v1.0.13-axon
RUN wget -q -O - https://deb.nodesource.com/setup_${NODE_VERSION}.x | bash - && apt-get install -y nodejs
RUN npm install --global npm@latest typescript@4.9.3
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
ENV GOLANG_VERSION=1.26.3
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
