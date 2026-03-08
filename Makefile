all: install
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
	docker build -t $(DOCKER_TAG) --progress=plain -f $< .
.PHONY: docker-build

install:
	$(MAKE) -C agent install
	$(MAKE) -C sdks/python install

.PHONY: install

test:
	$(MAKE) -C agent test
	@echo "TODO: sdk go test"
	$(MAKE) -C sdks/go test
	$(MAKE) -C scaffold test
	$(MAKE) -C agent relay-test
	$(MAKE) -C examples test
	$(MAKE) -C sdks/python test

publish:
	$(MAKE) -C sdks publish-go-sdk

.PHONY: publish

	