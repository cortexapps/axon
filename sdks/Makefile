
PUBLISH_GO_DIR ?= /tmp/cortex-axon-sdk-go
TARGET_GO_REPO ?= git@github.com:cortexapps/axon-go.git
CURRENT_SHA ?= $(shell git rev-parse HEAD)
PUBLISH_NAME ?= $(USER)

publish-go-sdk:
	$(MAKE) -C go
	@echo "Publishing SDK to $(PUBLISH_GO_DIR)"
	@rm -rf $(PUBLISH_GO_DIR)
	@git clone $(TARGET_GO_REPO) $(PUBLISH_GO_DIR)
	@cd go && cp -r . $(PUBLISH_GO_DIR)
	@cd $(PUBLISH_GO_DIR) && \
	if [ -n "$$(git status --porcelain)" ]; then \
		echo "Changes detected, publishing..."; \
		echo "$$(git status --porcelain)"; \
		git checkout -b "publish-$(PUBLISH_NAME)-$(CURRENT_SHA)" && \
		git add . && \
		git commit -m "Update SDK ($(CURRENT_SHA))" && \
		git push -f; \
	else \
		echo "No changes to publish"; \
	fi
