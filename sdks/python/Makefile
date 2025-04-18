all: clean install proto

install: .venv
.PHONY: install

.venv: pyproject.toml
	@echo "Installing python dependencies"
	@if ! which poetry >/dev/null; then \
		curl -sSL https://install.python-poetry.org | python3 -; \
	fi
	poetry install --no-root
	$(MAKE) proto
	poetry install
	touch .venv

lint: .venv
	poetry run ruff check
.PHONY: lint

fix: .venv
	poetry run ruff check --fix
.PHONY: fix

test: .venv generated/common_pb2.py generated/cortex_axon_agent_pb2.py generated/cortex_axon_agent_pb2_grpc.py generated/cortex_api_pb2.py generated/cortex_api_pb2_grpc.py
	poetry run pytest
.PHONY: test

proto: generated/common_pb2.py generated/cortex_axon_agent_pb2.py generated/cortex_api_pb2.py
.PHONY: proto

PROTO_FILES := $(wildcard ../agent/proto/*.proto)

generated/common_pb2.py generated/cortex_axon_agent_pb2.py generated/cortex_axon_agent_pb2_grpc.py: ../../agent/proto/cortex-axon-agent.proto ../../agent/proto/common.proto
	mkdir -p generated
	poetry run python -m grpc_tools.protoc -I $(dir $<) \
	    --python_out=$(dir $@) --grpc_python_out=$(dir $@) --pyi_out=$(dir $@) $^
	poetry run protol --create-package --in-place --python-out=$(dir $@) \
	    protoc --proto-path=$(dir $<) $^

generated/cortex_api_pb2.py generated/cortex_api_pb2_grpc.py: ../../agent/proto/cortex-api.proto ../../agent/proto/common.proto
	mkdir -p generated
	poetry run python -m grpc_tools.protoc -I $(dir $<) \
	    --python_out=$(dir $@) --grpc_python_out=$(dir $@) --pyi_out=$(dir $@) $^
	poetry run protol --create-package --in-place --python-out=$(dir $@) \
	    protoc --proto-path=$(dir $<) $^

example:
	poetry run example

clean:
	rm -rf generated
.PHONY: clean example
