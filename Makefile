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
# Extra `--build-arg`s, e.g. the Node 22 build below.
DOCKER_BUILD_ARGS ?=

docker-build: docker/Dockerfile
	docker build -t $(DOCKER_TAG) --build-arg VARIANT=$(DOCKER_VARIANT) $(DOCKER_BUILD_ARGS) --progress=plain -f $< .
.PHONY: docker-build

docker-build-grpc:
	$(MAKE) docker-build DOCKER_VARIANT=grpc DOCKER_TAG=cortex-axon-agent:local-grpc
.PHONY: docker-build-grpc

# The full image on Node 22 (published as -node22); see deps-full in docker/Dockerfile.
docker-build-node22:
	$(MAKE) docker-build DOCKER_TAG=cortex-axon-agent:local-node22 \
		DOCKER_BUILD_ARGS="--build-arg NODE_VERSION=22 --build-arg NODE_MIN_VERSION=22.23.4"
.PHONY: docker-build-node22

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

	