all: setup
	make -C agent
	make -C sdks/python

proto:
	$(MAKE) -C agent proto
	$(MAKE) -C server proto
	$(MAKE) -C sdks/python proto
.PHONY: proto

run-agent:
	$(MAKE) -C agent run
	
.PHONY: run-agent

DOCKER_TAG ?= cortex-axon-agent:local
# full or grpc (go-only, no Node); see the top of docker/Dockerfile.
DOCKER_VARIANT ?= full
# Extra `--build-arg`s, e.g. NODE_VERSION (see deps-full in docker/Dockerfile).
DOCKER_BUILD_ARGS ?=

docker-build: docker/Dockerfile
	docker build -t $(DOCKER_TAG) --build-arg VARIANT=$(DOCKER_VARIANT) $(DOCKER_BUILD_ARGS) --progress=plain -f $< .
.PHONY: docker-build

docker-build-grpc:
	$(MAKE) docker-build DOCKER_VARIANT=grpc DOCKER_TAG=cortex-axon-agent:local-grpc
.PHONY: docker-build-grpc

setup:
	$(MAKE) -C agent setup

.PHONY: setup

test:
	$(MAKE) -C agent test
	$(MAKE) -C server test
	$(MAKE) -C plugins/google-adc test test-race
	@echo "TODO: sdk go test"
	$(MAKE) -C sdks/go test
	$(MAKE) -C scaffold test
	$(MAKE) -C agent relay-test
	$(MAKE) -C examples test
	$(MAKE) -C sdks/python test

publish:
	$(MAKE) -C sdks publish-go-sdk

.PHONY: publish

	