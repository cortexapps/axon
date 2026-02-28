FROM debian:stable-slim
WORKDIR /agent

# Install dependencies
RUN apt update && apt install -y protobuf-compiler git python3 python3-venv wget build-essential openssl jq

# Install NodeJS and Snyk Broker
ENV NODE_VERSION=20
ENV FORCE_WS_RESPONSE=true
ARG SNYK_BROKER_VERSION=c62be461eb395c9274c4c90469bbf0788f08fe57
RUN wget -q -O - https://deb.nodesource.com/setup_${NODE_VERSION}.x | bash - && apt install -y nodejs
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

ENV GOLANG_VERSION=1.25.7
# Install Go, this script will work for ARM or AMD64
RUN wget -q -O /tmp/goinstall.sh https://raw.githubusercontent.com/canha/golang-tools-install-script/master/goinstall.sh && chmod +x /tmp/goinstall.sh && /tmp/goinstall.sh --version ${GOLANG_VERSION}

# Build the agentco
# ENV GOROOT="/root/.go"
ENV GOPATH="/root/.go"
ENV GOPROXY="https://proxy.golang.org,direct"
ENV PATH="${GOPATH}/bin:${PATH}:/agent"

COPY agent/. /build/.
RUN make -C /build setup proto
RUN cd /build && go build -o /agent/cortex-axon-agent
RUN rm -rf /build

COPY agent/server/snykbroker/accept_files /agent/accept_files
ENV ACCEPTFILE_DIR=/agent/accept_files

COPY agent/server/snykbroker/plugins /agent/plugins
ENV BUILTIN_PLUGIN_DIR=/agent/plugins

COPY /scaffold/. /app/scaffold/.

COPY docker/start /agent/start
COPY docker/entrypoint.sh /agent/entrypoint.sh
COPY docker/app_entrypoint.sh /app/app_entrypoint.sh
COPY docker/scripts /agent/scripts

ENV CORTEX_API_BASE_URL=https://api.getcortexapp.com
ENV HANDLER_HISTORY_PATH=/var/log/axon/history
ENV NODE_OPTIONS=--use-openssl-ca

EXPOSE 50051

ENTRYPOINT [ "/agent/entrypoint.sh" ]
CMD [ "serve" ]
