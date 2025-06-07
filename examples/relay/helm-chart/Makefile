test-render: 
	@helm template axon-render-test -f test/values.yaml . >/tmp/axon-render-test.yaml
	@if ! diff /tmp/axon-render-test.yaml test/expected.yaml; then \
		echo "Test failed: /tmp/axon-render-test.yaml does not match test/expected.yaml"; \
		exit 1; \
	else \
		echo "Test passed: /tmp/axon-render-test.yaml matches test/expected.yaml"; \
	fi

update-test:
	@helm template axon-render-test -f test/values.yaml . >/tmp/axon-render-test.yaml
	@echo "Updating test/expected.yaml with the latest output"
	@cp /tmp/axon-render-test.yaml test/expected.yaml
.PHONY: test