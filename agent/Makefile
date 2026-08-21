GO_SDK_DIR = ../sdks/go/
PYTHON_SDK_DIR = ../sdks/python/
GENERATED_DIR = .generated/proto
GENERATED_PATH=$(GENERATED_DIR)/github.com/cortexapps/axon

all: setup proto build test

PROTO_FILES := $(wildcard proto/*.proto)
GO_FILES := $(patsubst proto/%.proto,$(GENERATED_PATH)/%.pb.go,$(PROTO_FILES))

GOPATH ?= $(HOME)/go
GOBIN ?= $(GOPATH)/bin

proto: setup $(GO_FILES) version

version: $(GO_SDK_DIR)/version/agentversion.txt $(PYTHON_SDK_DIR)/cortex_axon/agentversion.py

$(GO_SDK_DIR)/version/agentversion.txt: common/grpcversion.txt
	@echo "Copying grpc version file to Go sdk"
	@mkdir -p $(GO_SDK_DIR)/version
	@cp $< $@

$(PYTHON_SDK_DIR)/cortex_axon/agentversion.py: common/grpcversion.txt
	@echo "Copying grpc version file to Python sdk"
	@mkdir -p $(PYTHON_SDK_DIR)/cortex_axon
	# write a python file with the grpc version eg "AGENT_VERSION = '0.0.1'"
	@cat $< | sed 's/^[ \t]*//;s/[ \t]*$$//' | sed 's/^/AGENT_VERSION = "/;s/$$/"\n/' > $@
	
$(GENERATED_PATH)/%.pb.go: proto/%.proto
	@echo "Generating protobuf: $< ==> $@ "
	@mkdir -p $(GENERATED_DIR)
	@protoc -I=proto --go_out=$(GENERATED_DIR) --go-grpc_out=$(GENERATED_DIR) $<
	@mkdir -p $(GO_SDK_DIR)/.generated
	@cp -R $(GENERATED_DIR) $(GO_SDK_DIR)/.generated

setup:
	@if ! command -v $(GOBIN)/protoc-gen-go >/dev/null; then echo "Installing protoc-gen-go..."; go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6; fi
	@if ! command -v $(GOBIN)/protoc-gen-go-grpc >/dev/null; then echo "Installing protoc-gen-go-grpc..."; go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1; fi
	@if ! which protoc >/dev/null; then \
		if which brew >/dev/null; then \
			brew install protobuf; \
		else \
			apt update && apt install -y protobuf-compiler; \
		fi; \
	fi

# Compiled plugins are separate Go modules, so their dependencies stay out of the
# agent binary. They build into a directory on PLUGIN_DIRS and are found by name.
# PLUGIN_SRC_DIR is overridable for the Docker build, which has no sibling
# ../plugins.
PLUGIN_SRC_DIR ?= ../plugins
PLUGIN_OUT_DIR ?= $(CURDIR)/plugins

plugins:
	@if [ ! -d "$(PLUGIN_SRC_DIR)/google-adc" ]; then \
		echo "no plugin sources at $(PLUGIN_SRC_DIR); set PLUGIN_SRC_DIR"; exit 1; \
	fi
	$(MAKE) -C $(PLUGIN_SRC_DIR)/google-adc build OUT_DIR=$(PLUGIN_OUT_DIR)
.PHONY: plugins

build: plugins
	go build -o axon-agent main.go

mockgen:
	mkdir -p test/mocks
	@if ! command -v $(GOBIN)/mockgen >/dev/null; then echo "Installing mockgen..."; go install go.uber.org/mock/mockgen@v0.6.0; fi
.PHONY: mockgen

server/http/http_server_mock.go: server/http/http_server.go
	@$(MAKE) mockgen
	$(GOBIN)/mockgen -package http  -destination $@ github.com/cortexapps/axon/server/http Server
	@echo "Mock $@ generated"

server/snykbroker/registration_mock.go: server/snykbroker/registration.go
	@$(MAKE) mockgen
	$(GOBIN)/mockgen -package snykbroker -destination $@ github.com/cortexapps/axon/server/snykbroker Registration  
	@echo "Mock $@ generated"

mocks: server/http/http_server_mock.go server/snykbroker/registration_mock.go

test: mocks
	go mod tidy
	go test -v ./...

# Packages that currently pass the race detector. The rest of the tree does
# not yet: server, server/handler, server/http, server/snykbroker and cmd all
# have known races (supervisor process lifecycle, handler manager, the fx
# server stack). Move packages up here as those are fixed.
RACE_CLEAN_PKGS := ./common/... ./config/... ./scaffold/... ./server/api/... ./server/cron/... ./server/snykbroker/acceptfile/... ./util/...

# The reflector lives in server/snykbroker, whose other tests are not yet
# race-clean, so it is gated at test granularity instead. Derived from the
# test files so new reflector tests are covered automatically.
REFLECTOR_TESTS = $(shell grep -ho '^func Test[A-Za-z0-9_]*' server/snykbroker/reflector*_test.go | sed 's/^func //' | paste -sd'|' -)

test-race: mocks
	go test -race $(RACE_CLEAN_PKGS)
	go test -race -run '^($(REFLECTOR_TESTS))$$' ./server/snykbroker/

.PHONY: test-race

# Live check of the Google credential source against whatever Application Default
# Credentials this machine has. Opt-in: it makes real requests to Google.
test-google-live: plugins
	AXON_GOOGLE_LIVE_TEST=1 go test -count=1 -run TestLiveGoogle -v ./server/snykbroker/
.PHONY: test-google-live

validate-filters:
	test/validate_filters.sh

relay-test-no-proxy:
	# No proxy case
	@echo "Running relay tests: no proxy"
	cd test/relay && export PROXY=0 && ./relay_test.sh

relay-test-with-proxy: 
	# Proxy case
	@echo "Running relay tests: with proxy"
	cd test/relay && export PROXY=1 && ./relay_test.sh

relay-test: relay-test-no-proxy relay-test-with-proxy

.PHONY: relay-test relay-test-no-proxy relay-test-with-proxy

run: proto
	go run main.go serve


.PHONY: setup test build 
