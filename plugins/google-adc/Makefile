BINARY ?= google-adc

# A plugin directory the agent searches. PLUGIN_DIRS defaults to ./plugins
# relative to the agent's working directory, which is where this lands for local
# runs; the image copies it into /agent/plugins alongside the shipped scripts.
OUT_DIR ?= ../../agent/plugins

build:
	go build -o $(OUT_DIR)/$(BINARY) .
.PHONY: build

test:
	go test ./...
.PHONY: test

# This module holds the token cache, whose whole job is to be safe under
# concurrent access from separate processes. It left the agent's RACE_CLEAN_PKGS
# by moving here, so the gate has to be re-established.
test-race:
	go test -race ./...
.PHONY: test-race

tidy:
	go mod tidy
.PHONY: tidy
