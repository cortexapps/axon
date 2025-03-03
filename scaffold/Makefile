OS=$(shell uname -s)

update-go-scaffold:
	@echo "Building proto"
	@cd ../agent && make proto
	@echo "Copying generated proto files to go scaffold"
	@rm -rf go/axon_client
	@mkdir -p go
	@cp -R ../sdks/go/ go/axon_client/
	@rm -rf go/axon_client/example
	@rm -rf go/axon_client/mock_axon
	@rm -rf go/axon_client/*_test.go

update-scaffolds: update-go-scaffold

test: update-go-scaffold update-python-scaffold
	@echo "Running tests"
	./scaffold_test.sh go
	./scaffold_test.sh python

.PHONY: update-go-scaffold update-python-scaffold test update-scaffolds
