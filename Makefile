HELM_CHART_DIR := charts/w7panel-appid-proxy
HELM_VALUES_FILE := $(HELM_CHART_DIR)/values.yaml
HELM_CHART_FILE := $(HELM_CHART_DIR)/Chart.yaml
HELM_PACKAGE_DIR ?= charts

HELM_IMAGE_REPOSITORY := $(shell awk '/^image:/{flag=1; next} flag && /^[^[:space:]]/{flag=0} flag && $$1=="repository:" {print $$2; exit}' $(HELM_VALUES_FILE))
HELM_IMAGE_TAG := $(shell awk '/^image:/{flag=1; next} flag && /^[^[:space:]]/{flag=0} flag && $$1=="tag:" {print $$2; exit}' $(HELM_VALUES_FILE))
HELM_CHART_VERSION ?= $(shell awk '$$1=="version:" {print $$2; exit}' $(HELM_CHART_FILE))

IMAGE_REPOSITORY ?= $(HELM_IMAGE_REPOSITORY)
IMAGE_TAG ?= $(HELM_IMAGE_TAG)
IMAGE ?= $(IMAGE_REPOSITORY):$(IMAGE_TAG)
HELM_APP_VERSION ?= $(IMAGE_TAG)
BETA_SUFFIX ?=
BETA_IMAGE_TAG ?= $(IMAGE_TAG)-$(BETA_SUFFIX)
BINARY ?= runtime/main

.PHONY: build publish go-build chart-package

build:
	docker build -t $(IMAGE) .

go-build:
	mkdir -p $(dir $(BINARY))
	go build -o $(BINARY) .

publish: build chart-package
	docker push $(IMAGE)

chart-package:
	mkdir -p $(HELM_PACKAGE_DIR)
	helm package $(HELM_CHART_DIR) --version $(HELM_CHART_VERSION) --app-version $(HELM_APP_VERSION) --destination $(HELM_PACKAGE_DIR)
