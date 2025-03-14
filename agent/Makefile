GO_SDK_DIR = ../sdks/go/
GENERATED_DIR = .generated/proto
GENERATED_PATH=$(GENERATED_DIR)/github.com/cortexapps/axon

all: setup proto build test

PROTO_FILES := $(wildcard proto/*.proto)
GO_FILES := $(patsubst proto/%.proto,$(GENERATED_PATH)/%.pb.go,$(PROTO_FILES))

GOPATH ?= $(HOME)/go
GOBIN ?= $(GOPATH)/bin

proto: setup $(GO_FILES)

$(GENERATED_PATH)/%.pb.go: proto/%.proto
	@echo "Generating protobuf: $< ==> $@ "
	@mkdir -p $(GENERATED_DIR)
	@protoc -I=proto --go_out=$(GENERATED_DIR) --go-grpc_out=$(GENERATED_DIR) $<
	@mkdir -p $(GO_SDK_DIR)/.generated
	@cp -R $(GENERATED_DIR) $(GO_SDK_DIR)/.generated

setup:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	@if ! which protoc </dev/null; then \
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
	@if ! command -v $(GOBIN)/mockgen >/dev/null; then echo "Installing mockgen..."; go install go.uber.org/mock/mockgen@latest; fi
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

relay-test:
	cd test && ./relay_test.sh

run: proto
	go run main.go serve


.PHONY: setup test build 
