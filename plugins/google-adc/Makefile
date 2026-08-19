BINARY ?= google-adc

# PLUGIN_DIRS defaults to ./plugins relative to the agent's working directory.
OUT_DIR ?= ../../agent/plugins

build:
	go build -o $(OUT_DIR)/$(BINARY) .
.PHONY: build

test:
	go test ./...
.PHONY: test

# The token cache has to be safe under concurrent access from separate processes.
# This module is outside the agent's RACE_CLEAN_PKGS, so it gates itself.
test-race:
	go test -race ./...
.PHONY: test-race

tidy:
	go mod tidy
.PHONY: tidy
