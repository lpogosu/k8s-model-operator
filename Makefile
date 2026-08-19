SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

# Pinned so that a run here and a run in CI use the same code generator, the same
# linter and the same API server.
CONTROLLER_TOOLS_VERSION ?= v0.19.0
GOLANGCI_LINT_VERSION    ?= v2.13.2
ENVTEST_K8S_VERSION      ?= 1.34.1

IMG ?= ghcr.io/lpogosu/k8s-model-operator:dev

LOCALBIN       := $(CURDIR)/bin
CONTROLLER_GEN := $(LOCALBIN)/controller-gen
SETUP_ENVTEST  := $(LOCALBIN)/setup-envtest
GOLANGCI_LINT  := $(LOCALBIN)/golangci-lint
ENVTEST_DIR    := $(LOCALBIN)/envtest

# The generator is pointed at packages, not at "./...": the repository root holds
# no Go files and the loader treats that as an error.
GEN_PATHS := paths=./api/v1alpha1 paths=./internal/controller

##@ General

.PHONY: help
help: ## List the available targets.
	@awk 'BEGIN {FS = ":.*##"} \
		/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n%s\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Code generation

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Regenerate the deepcopy functions.
	$(CONTROLLER_GEN) object $(GEN_PATHS)

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Regenerate the CRD and the RBAC role.
	$(CONTROLLER_GEN) crd rbac:roleName=model-operator $(GEN_PATHS) \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac

.PHONY: check-generated
check-generated: manifests generate ## Fail if the generated files are out of date.
	@git diff --exit-code -- api config \
		|| { echo "generated files are stale: run 'make manifests generate'"; exit 1; }

##@ Checks

.PHONY: fmt
fmt: ## Format the Go sources.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run golangci-lint.
	$(GOLANGCI_LINT) run ./...

.PHONY: test
test: manifests generate $(SETUP_ENVTEST) ## Run every test, including the envtest suite.
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_DIR) -p path)" \
		go test ./... -race -count=1

.PHONY: verify
verify: fmt vet lint test ## Everything CI runs.

##@ Build

.PHONY: build
build: ## Build the manager binary into bin/.
	go build -o $(LOCALBIN)/manager ./cmd

.PHONY: run
run: manifests generate ## Run the manager against the cluster in the current kubecontext.
	go run ./cmd

.PHONY: docker-build
docker-build: ## Build the operator image.
	docker build -t $(IMG) .

##@ Deployment

.PHONY: install
install: manifests ## Install the CRD into the current cluster.
	kubectl apply -f config/crd/bases

.PHONY: uninstall
uninstall: ## Remove the CRD, and with it every ModelDeployment.
	kubectl delete --ignore-not-found -f config/crd/bases

.PHONY: deploy
deploy: manifests ## Install the CRD, the RBAC and the manager Deployment.
	kubectl apply -k config/default

.PHONY: undeploy
undeploy: ## Remove everything deploy installed.
	kubectl delete --ignore-not-found -k config/default

.PHONY: sample
sample: ## Apply the sample ModelDeployments.
	kubectl apply -k config/samples

##@ Tools

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

$(CONTROLLER_GEN): | $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

$(SETUP_ENVTEST): | $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.25

$(GOLANGCI_LINT): | $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: clean
clean: ## Delete the downloaded tools and build output.
	rm -rf $(LOCALBIN)
