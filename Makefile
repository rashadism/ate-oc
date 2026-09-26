IMG ?= oc-substrate:dev
CONTROLLER_TOOLS_VERSION ?= v0.21.0
GOLANGCI_LINT_VERSION ?= v2.8.0

LOCALBIN ?= $(shell pwd)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint

.PHONY: all
all: build

.PHONY: generate
generate: controller-gen
	$(CONTROLLER_GEN) object paths="./api/..."
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:artifacts:config=helm/crds

.PHONY: unit-test
unit-test:
	go test ./...

.PHONY: lint
lint: golangci-lint
	$(GOLANGCI_LINT) run

.PHONY: build
build:
	go build -o bin/operator ./cmd/operator
	go build -o bin/frontdoor ./cmd/frontdoor

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: helm-template
helm-template:
	helm template oc-substrate helm --include-crds >/dev/null

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

.PHONY: controller-gen
controller-gen: $(LOCALBIN)
	@test -x $(CONTROLLER_GEN) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: golangci-lint
golangci-lint: $(LOCALBIN)
	@test -x $(GOLANGCI_LINT) || GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
