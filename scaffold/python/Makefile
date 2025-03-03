IMAGE_NAME ?= ghcr.io/cortexapps/cortex-axon-agent:latest

docker:
	docker build  --build-arg "IMAGE_NAME=$(IMAGE_NAME)" -t {{.ProjectName | toLower }}:local .

dryrun: docker
	docker run -e "DRYRUN=1" --rm {{.ProjectName | toLower }}:local


run: docker
	docker run -e "CORTEX_API_TOKEN=$(CORTEX_API_TOKEN)" --rm {{.ProjectName | toLower }}:local
