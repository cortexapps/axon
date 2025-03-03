all: setup
	make -C agent
	make -C sdks/python

proto:
	$(MAKE) -C agent proto
	$(MAKE) -C sdks/python proto
.PHONY: proto

run-agent:
	$(MAKE) -C agent run
	
.PHONY: run-agent

DOCKER_TAG ?= cortex-axon-agent:local

docker-build: docker/Dockerfile
	docker build -t $(DOCKER_TAG) -f $< .
.PHONY: docker-build

setup:
	$(MAKE) -C agent setup

.PHONY: setup

test:
	$(MAKE) -C agent test
	$(MAKE) -C sdks/python test
	@echo "TODO: sdk go test"
	$(MAKE) -C sdks/go test
	$(MAKE) -C scaffold test
	$(MAKE) -C agent relay-test
	$(MAKE) -C examples test

publish:
	$(MAKE) -C sdks publish-go-sdk

.PHONY: publish

	