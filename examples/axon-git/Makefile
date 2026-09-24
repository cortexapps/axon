IMAGE_NAME ?= ghcr.io/cortexapps/cortex-axon-agent:latest

docker:
	docker build  --build-arg "IMAGE_NAME=$(IMAGE_NAME)" -t axon-git:local .

dryrun: docker
	docker run -e "DRYRUN=1" --rm axon-git:local


run: docker
	docker run -e "CORTEX_API_TOKEN=$(CORTEX_API_TOKEN)" --rm axon-git:local
