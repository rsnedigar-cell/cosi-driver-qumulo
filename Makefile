IMAGE ?= ghcr.io/rsnedigar-cell/cosi-driver-qumulo
TAG ?= 0.2.0
VERSION ?= $(TAG)
GO ?= go
BIN_DIR ?= bin

.PHONY: all build build-cosi build-csi test race vet lint image integration kind-e2e tidy

all: vet test build

tidy:
	$(GO) mod tidy

build: build-cosi build-csi

$(BIN_DIR):
	mkdir -p $(BIN_DIR)

build-cosi: | $(BIN_DIR)
	$(GO) build -trimpath -ldflags="-X main.version=$(VERSION)" -o $(BIN_DIR)/qumulo-cosi-driver ./cmd/qumulo-cosi-driver

build-csi: | $(BIN_DIR)
	$(GO) build -trimpath -ldflags="-X main.version=$(VERSION)" -o $(BIN_DIR)/qumulo-csi-driver ./cmd/qumulo-csi-driver

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

lint:
	golangci-lint run ./...

image:
	docker build --platform linux/amd64 --target cosi --build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG) .
	docker build --platform linux/amd64 --target csi --build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG)-csi .

integration:
	$(GO) test -tags=integration ./test/integration

kind-e2e:
	@echo "control-plane e2e: kind + COSI v0.2.2 + fake Qumulo â€” see test/e2e"
	$(GO) test ./test/e2e
