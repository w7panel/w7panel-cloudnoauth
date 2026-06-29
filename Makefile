IMAGE_REPOSITORY ?= zpk.idc.w7.com/we7team/zpk-market
IMAGE_TAG ?= latest
IMAGE ?= $(IMAGE_REPOSITORY):$(IMAGE_TAG)
BINARY ?= runtime/main

.PHONY: build publish go-build

build:
	docker build -t $(IMAGE) .

go-build:
	mkdir -p $(dir $(BINARY))
	go build -o $(BINARY) .

publish: build
	docker push $(IMAGE)
