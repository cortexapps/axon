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

build:
	go build -o axon-agent main.go

mockgen:
	mkdir -p test/mocks
	@if ! command -v $(GOBIN)/mockgen >/dev/null; then echo "Installing mockgen..."; go install go.uber.org/mock/mockgen@v0.5.0; fi
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

validate-filters:
	test/validate_filters.sh

relay-test:
	cd test && ./relay_test.sh

run: proto
	go run main.go serve


.PHONY: setup test build 
