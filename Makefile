# Image URL to use all building/pushing image targets
IMG ?= ghcr.io/braghettos/cloudburst-autoscaler:latest
# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.29.1

ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

CONTAINER_TOOL ?= docker

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd:allowDangerousTypes=true paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test ./... -coverprofile cover.out

.PHONY: lint
lint: ## Run golangci-lint.
	golangci-lint run ./...

##@ Build

.PHONY: build
build: generate fmt vet ## Build manager binary.
	CGO_ENABLED=0 go build -o bin/manager main.go

.PHONY: run
run: manifests generate fmt vet ## Run controller from your host.
	go run ./main.go

.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

.PHONY: kind-load
kind-load: docker-build ## Load docker image into Kind cluster (for local development).
	@if ! command -v kind >/dev/null 2>&1; then \
		echo "Error: kind is not installed"; \
		exit 1; \
	fi
	@if ! kind get clusters 2>/dev/null | grep -q "^$(KIND_CLUSTER_NAME)$$"; then \
		echo "Error: Kind cluster '$(KIND_CLUSTER_NAME)' not found"; \
		echo "Available clusters: $$(kind get clusters 2>/dev/null || echo 'none')"; \
		exit 1; \
	fi
	@echo "Loading image $(IMG) into Kind cluster '$(KIND_CLUSTER_NAME)'..."
	@kind load docker-image ${IMG} --name $(KIND_CLUSTER_NAME) || \
		(docker save ${IMG} | docker exec -i $(KIND_CLUSTER_NAME)-control-plane ctr --namespace=k8s.io images import - && \
		 docker save ${IMG} | docker exec -i $(KIND_CLUSTER_NAME)-worker ctr --namespace=k8s.io images import - 2>/dev/null || true)
	@echo "✅ Image loaded into Kind cluster"

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

# Helper function to read value from .env file
# Handles: comments, empty lines, quoted values, spaces around =
# Usage: $(call get_env,TAILSCALE_AUTHKEY)
get_env = $(shell if [ -f .env ]; then \
	grep -E "^[[:space:]]*$(1)[[:space:]]*=" .env 2>/dev/null | \
	grep -v "^[[:space:]]*#" | \
	head -1 | \
	sed 's/^[^=]*=[[:space:]]*//' | \
	sed 's/[[:space:]]*$$//' | \
	sed "s/^['\"]//" | \
	sed "s/['\"]$$//"; \
	fi)

# Deployment configuration
# Values are read from .env file, but can be overridden via command line
# Priority: command line > .env file
GCP_CREDENTIALS_FILE ?= $(call get_env,GOOGLE_APPLICATION_CREDENTIALS)
TAILSCALE_AUTHKEY ?= $(call get_env,TAILSCALE_AUTHKEY)
NAMESPACE ?= cloudburst-system
KIND_CLUSTER_NAME ?= cloudburst

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: create-namespace
create-namespace: ## Create the cloudburst-system namespace.
	$(KUBECTL) create namespace $(NAMESPACE) --dry-run=client -o yaml | $(KUBECTL) apply -f -

.PHONY: create-secrets
create-secrets: create-namespace ## Create required secrets (GCP credentials and Tailscale auth key). Reads from .env file or command line.
	@if [ -z "$(GCP_CREDENTIALS_FILE)" ] || [ ! -f "$(GCP_CREDENTIALS_FILE)" ]; then \
		echo "Error: GCP credentials file not found"; \
		echo "  Expected at: $(GCP_CREDENTIALS_FILE)"; \
		echo ""; \
		echo "  Options:"; \
		echo "    1. Set GOOGLE_APPLICATION_CREDENTIALS in .env file"; \
		echo "    2. Override: make create-secrets GCP_CREDENTIALS_FILE=/path/to/gcp-sa.json"; \
		echo "    3. Place file at: ../cloudbroker/secrets/gcp-sa.json"; \
		exit 1; \
	fi
	@if [ -z "$(TAILSCALE_AUTHKEY)" ]; then \
		echo "Error: TAILSCALE_AUTHKEY not set"; \
		echo "  Options:"; \
		echo "    1. Set TAILSCALE_AUTHKEY in .env file"; \
		echo "    2. Override: make create-secrets TAILSCALE_AUTHKEY=tskey-auth-..."; \
		exit 1; \
	fi
	@echo "Creating GCP credentials secret..."
	$(KUBECTL) create secret generic gcp-credentials \
		--from-file=credentials.json=$(GCP_CREDENTIALS_FILE) \
		-n $(NAMESPACE) \
		--dry-run=client -o yaml | $(KUBECTL) apply -f -
	@echo "Creating Tailscale auth key secret..."
	$(KUBECTL) create secret generic tailscale-auth \
		--from-literal=authkey=$(TAILSCALE_AUTHKEY) \
		-n $(NAMESPACE) \
		--dry-run=client -o yaml | $(KUBECTL) apply -f -
	@echo "✅ Secrets created successfully!"

.PHONY: deploy-all
deploy-all: create-secrets install deploy ## Deploy everything: create secrets, install CRDs, and deploy controller.
	@echo ""
	@echo "✅ Deployment complete!"
	@echo ""
	@echo "Verify deployment with:"
	@echo "  kubectl get pods -n $(NAMESPACE)"
	@echo "  kubectl logs -n $(NAMESPACE) -l control-plane=controller-manager"

.PHONY: build-and-deploy
build-and-deploy: docker-build ## Build Docker image, load/push, and deploy everything. Auto-detects Kind cluster for local loading.
	@if command -v kind >/dev/null 2>&1 && kind get clusters 2>/dev/null | grep -q "^$(KIND_CLUSTER_NAME)$$"; then \
		echo "Detected Kind cluster '$(KIND_CLUSTER_NAME)', loading image locally..."; \
		kind load docker-image ${IMG} --name $(KIND_CLUSTER_NAME); \
	else \
		echo "No Kind cluster detected, pushing to registry..."; \
		$(CONTAINER_TOOL) push ${IMG}; \
	fi
	@$(MAKE) deploy-all NAMESPACE=$(NAMESPACE) GCP_CREDENTIALS_FILE=$(GCP_CREDENTIALS_FILE) TAILSCALE_AUTHKEY=$(TAILSCALE_AUTHKEY)
	@echo ""
	@echo "🎉 Build and deployment complete!"
	@echo ""
	@echo "Image: $(IMG)"
	@echo "Namespace: $(NAMESPACE)"
	@echo ""
	@echo "Verify deployment with:"
	@echo "  kubectl get pods -n $(NAMESPACE)"
	@echo "  kubectl logs -n $(NAMESPACE) -l control-plane=controller-manager"

.PHONY: create-kind-cluster
create-kind-cluster: ## Create a Kind cluster with a worker node.
	@if ! command -v kind >/dev/null 2>&1; then \
		echo "Error: kind is not installed. Install it from https://kind.sigs.k8s.io/"; \
		exit 1; \
	fi
	@if kind get clusters 2>/dev/null | grep -q "^$(KIND_CLUSTER_NAME)$$"; then \
		echo "Kind cluster '$(KIND_CLUSTER_NAME)' already exists. Skipping creation."; \
		echo "To recreate, delete it first: kind delete cluster --name $(KIND_CLUSTER_NAME)"; \
	else \
		echo "Creating Kind cluster '$(KIND_CLUSTER_NAME)' with worker node..."; \
		if [ -f "./hack/create-kind.sh" ]; then \
			HOST_TS_IP=127.0.0.1 KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) ./hack/create-kind.sh; \
		else \
			echo "Using inline Kind config (hack/create-kind.sh not found)..."; \
			echo "kind: Cluster" > /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "apiVersion: kind.x-k8s.io/v1alpha4" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "networking:" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "  apiServerAddress: \"0.0.0.0\"" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "  apiServerPort: 6443" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "nodes:" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "- role: control-plane" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "  image: kindest/node:v1.34.3" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "  kubeadmConfigPatches:" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "  - |" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "    kind: InitConfiguration" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "    nodeRegistration:" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "      kubeletExtraArgs:" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "        node-labels: \"cloudburst.io/node=control-plane\"" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "- role: worker" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "  image: kindest/node:v1.34.3" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "  kubeadmConfigPatches:" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "  - |" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "    kind: JoinConfiguration" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "    nodeRegistration:" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "      kubeletExtraArgs:" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			echo "        node-labels: \"cloudburst.io/node=controller-worker\"" >> /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			kind create cluster --name $(KIND_CLUSTER_NAME) --config /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
			rm -f /tmp/kind-config-$(KIND_CLUSTER_NAME).yaml; \
		fi \
	fi
	@echo "✓ Kind cluster '$(KIND_CLUSTER_NAME)' is ready"
	@kubectl config set-cluster kind-$(KIND_CLUSTER_NAME) --server="https://127.0.0.1:6443" 2>/dev/null || true

.PHONY: kind-setup-and-deploy
kind-setup-and-deploy: create-kind-cluster docker-build kind-load deploy-all ## Create Kind cluster with worker node, build image, load locally, and deploy everything.
	@echo ""
	@echo "🎉 Complete Kind setup and deployment finished!"
	@echo ""
	@echo "Kind Cluster: $(KIND_CLUSTER_NAME)"
	@echo "Image: $(IMG)"
	@echo "Namespace: $(NAMESPACE)"
	@echo ""
	@echo "Verify deployment with:"
	@echo "  kubectl get nodes"
	@echo "  kubectl get pods -n $(NAMESPACE)"
	@echo "  kubectl logs -n $(NAMESPACE) -l control-plane=controller-manager"

##@ Tools

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
KUBECTL ?= kubectl

KUSTOMIZE_VERSION ?= v5.0.0
CONTROLLER_TOOLS_VERSION ?= v0.20.0

KUSTOMIZE_INSTALL_SCRIPT ?= "https://raw.githubusercontent.com/kubernetes-sigs/kustomize/master/hack/install_kustomize.sh"
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	@if test -x $(LOCALBIN)/kustomize && ! $(LOCALBIN)/kustomize version | grep -q $(KUSTOMIZE_VERSION); then \
		echo "$(LOCALBIN)/kustomize version is not $(KUSTOMIZE_VERSION). Removing it before installing."; \
		rm -f $(LOCALBIN)/kustomize; \
	fi
	test -s $(LOCALBIN)/kustomize || { curl -Ss $(KUSTOMIZE_INSTALL_SCRIPT) | bash -s -- $(subst v,,$(KUSTOMIZE_VERSION)) $(LOCALBIN); }

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	test -s $(LOCALBIN)/controller-gen || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: envtest
envtest: $(ENVTEST) ## Download envtest-setup locally if necessary.
$(ENVTEST): $(LOCALBIN)
	test -s $(LOCALBIN)/setup-envtest || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
