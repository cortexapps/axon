

GOROOT ?= $(shell go env GOROOT)
GOPATH ?= $(shell go env GOPATH)

mockgen:
	@if ! command -v $(GOPATH)/bin/mockgen >/dev/null; then echo "Installing mockgen..."; go install go.uber.org/mock/mockgen@latest; fi
.PHONY: mockgen

mock_axon/mocks.go: $(wildcard .generated/proto/github.com/cortexapps/axon/*.go)
	@$(MAKE) mockgen
	@echo "Generating mocks for axon"
	@mkdir -p mock_axon
	$(GOPATH)/bin/mockgen github.com/cortexapps/axon-go/.generated/proto/github.com/cortexapps/axon CortexApiClient,AxonAgentClient >mock_axon/mocks.go
	@echo "Mocks generated"

test: mock_axon/mocks.go
	@go mod tidy
	go test -v ./...
