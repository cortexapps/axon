PROTO_DIR = ../proto/tunnel
GENERATED_DIR = .generated/proto
GENERATED_PATH = $(GENERATED_DIR)/tunnel

GOPATH ?= $(HOME)/go
GOBIN ?= $(GOPATH)/bin

PROTO_FILES := $(wildcard $(PROTO_DIR)/*.proto)
GO_FILES := $(patsubst $(PROTO_DIR)/%.proto,$(GENERATED_PATH)/%.pb.go,$(PROTO_FILES))

all: setup proto build

proto: setup $(GO_FILES)

$(GENERATED_PATH)/%.pb.go: $(PROTO_DIR)/%.proto
	@echo "Generating protobuf: $< ==> $@"
	@mkdir -p $(GENERATED_DIR)
	protoc -I=$(PROTO_DIR) \
		--go_out=$(GENERATED_DIR) --go_opt=module=github.com/cortexapps/axon-server \
		--go-grpc_out=$(GENERATED_DIR) --go-grpc_opt=module=github.com/cortexapps/axon-server \
		$<

setup:
	@if ! command -v $(GOBIN)/protoc-gen-go >/dev/null; then echo "Installing protoc-gen-go..."; go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6; fi
	@if ! command -v $(GOBIN)/protoc-gen-go-grpc >/dev/null; then echo "Installing protoc-gen-go-grpc..."; go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1; fi

build: proto
	go build -o axon-server ./cmd/...

test:
	go test -v ./...

clean:
	rm -rf $(GENERATED_DIR) axon-server

.PHONY: all setup proto build test clean
