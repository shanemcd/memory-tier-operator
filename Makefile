# memory-tier-operator build
REGISTRY ?= ghcr.io/shanemcd
IMAGE ?= $(REGISTRY)/memory-tier-operator
TAG ?= latest

.PHONY: build test vet container push deploy clean

build:
	go build -o bin/operator ./cmd/operator
	go build -o bin/driver ./cmd/driver

test:
	go test ./...

vet:
	go vet ./...

container:
	CGO_ENABLED=0 podman build -t $(IMAGE):$(TAG) .

push:
	podman push $(IMAGE):$(TAG)

clean:
	rm -rf bin/
