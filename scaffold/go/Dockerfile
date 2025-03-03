FROM {{.DockerImage}}

ARG GOPRIVATE="github.com/cortexapps"
ARG GITHUB_ID
ENV GITHUB_ID=$GITHUB_ID

WORKDIR /project
COPY . . 

# Temporary until SDK is public
RUN --mount=type=secret,id=github_token \
    export GITHUB_TOKEN=$(head -n 1 /run/secrets/github_token) && \
    echo "machine github.com login x password $GITHUB_TOKEN" > ~/.netrc && \
    git config --global url."https://${GITHUB_ID}:${GITHUB_TOKEN}@github.com/".insteadOf "https://github.com/"

RUN go mod tidy
RUN go build -o /project/cortex-axon-app .

ENTRYPOINT [ "/app/app_entrypoint.sh", "/project/cortex-axon-app" ]