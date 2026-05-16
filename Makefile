# Image URL to use all building/pushing image targets
IMG ?= controller:latest

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
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
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

KIND_CLUSTER ?= global-workload-orchestrator-test-e2e

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name global-workload-orchestrator-builder
	$(CONTAINER_TOOL) buildx use global-workload-orchestrator-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm global-workload-orchestrator-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint

KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.20.1
GOLANGCI_LINT_VERSION ?= v2.8.0

ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?([0-9]+)\.([0-9]+).*/release-\1.\2/')

ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.

GOLANGCI_LINT_BOOTSTRAP := $(LOCALBIN)/golangci-lint-bootstrap-$(GOLANGCI_LINT_VERSION)

$(GOLANGCI_LINT_BOOTSTRAP): | $(LOCALBIN)
	@echo "Installing bootstrap golangci-lint $(GOLANGCI_LINT_VERSION)..."
	GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	mv $(LOCALBIN)/golangci-lint $(GOLANGCI_LINT_BOOTSTRAP)

$(GOLANGCI_LINT): $(GOLANGCI_LINT_BOOTSTRAP) .custom-gcl.yml | $(LOCALBIN)
	@echo "Building custom golangci-lint with plugins..."
	$(GOLANGCI_LINT_BOOTSTRAP) custom \
	    --destination $(LOCALBIN) \
	    --name golangci-lint-custom
	mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT)

define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef

##@ Demo

KUBECONFIG_DIR ?= hack/kubeconfigs
DEMO_NS ?= default

.PHONY: demo-setup
demo-setup: ## Provision three kind clusters, kubeconfigs, secrets, registrations
	@echo "==> Creating kind clusters..."
	kind create cluster --name mgmt || true
	kind create cluster --name region-a || true
	kind create cluster --name region-b || true
	@echo "==> Exporting kubeconfigs..."
	mkdir -p $(KUBECONFIG_DIR)
	kind get kubeconfig --name region-a > $(KUBECONFIG_DIR)/region-a.kubeconfig
	kind get kubeconfig --name region-b > $(KUBECONFIG_DIR)/region-b.kubeconfig
	@echo "==> Switching to mgmt context..."
	kubectl config use-context kind-mgmt
	@echo "==> Installing CRDs into mgmt..."
	$(MAKE) install
	@echo "==> Creating kubeconfig secrets..."
	kubectl create secret generic region-a-kubeconfig \
		--from-file=kubeconfig=$(KUBECONFIG_DIR)/region-a.kubeconfig \
		--dry-run=client -o yaml | kubectl apply -f -
	kubectl create secret generic region-b-kubeconfig \
		--from-file=kubeconfig=$(KUBECONFIG_DIR)/region-b.kubeconfig \
		--dry-run=client -o yaml | kubectl apply -f -
	@echo "==> Registering clusters..."
	kubectl apply -f hack/clusters.yaml
	@echo "==> Setup complete. Run 'make run' in another terminal, then 'make demo'."

.PHONY: demo
demo: ## Apply a sample GlobalWorkload and show placement across clusters
	@echo "==> Applying sample GlobalWorkload..."
	kubectl apply -f hack/sample-workload.yaml
	@echo "==> Waiting for placement (5s)..."
	@sleep 5
	@echo ""
	@echo "==> GlobalWorkload status:"
	kubectl get globalworkload hello-workload -o yaml | grep -A 20 'status:'
	@echo ""
	@echo "==> Deployments in region-a:"
	-kubectl --kubeconfig $(KUBECONFIG_DIR)/region-a.kubeconfig get deployment -n $(DEMO_NS) gwo-hello-workload
	@echo ""
	@echo "==> Deployments in region-b:"
	-kubectl --kubeconfig $(KUBECONFIG_DIR)/region-b.kubeconfig get deployment -n $(DEMO_NS) gwo-hello-workload

.PHONY: demo-failover
demo-failover: ## Stop region-a's container to simulate a cluster failure
	@echo "==> Stopping region-a's API server..."
	docker stop region-a-control-plane
	@echo ""
	@echo "==> Watching for region-a to be marked unhealthy (up to 60s)..."
	@for i in $$(seq 1 12); do \
		HEALTHY=$$(kubectl get clusterregistration region-a -o jsonpath='{.status.healthy}' 2>/dev/null); \
		if [ "$$HEALTHY" = "false" ]; then \
			echo "    region-a is now UNHEALTHY (after $$((i*5))s)"; \
			break; \
		fi; \
		echo "    [$$((i*5))s] region-a healthy=$$HEALTHY, waiting..."; \
		sleep 5; \
	done
	@echo ""
	@echo "==> Waiting for migration to complete (10s)..."
	@sleep 10
	@echo ""
	@echo "==> Updated placement:"
	kubectl get globalworkload hello-workload -o jsonpath='{.status.placements}' | jq
	@echo ""
	@echo "==> Replicas now in region-b:"
	-kubectl --kubeconfig $(KUBECONFIG_DIR)/region-b.kubeconfig get deployment -n $(DEMO_NS) gwo-hello-workload

.PHONY: demo-recover
demo-recover: ## Restart region-a and observe redistribution
	@echo "==> Starting region-a's API server..."
	docker start region-a-control-plane
	@echo ""
	@echo "==> Waiting for region-a to be marked healthy (up to 60s)..."
	@for i in $$(seq 1 12); do \
		HEALTHY=$$(kubectl get clusterregistration region-a -o jsonpath='{.status.healthy}' 2>/dev/null); \
		if [ "$$HEALTHY" = "true" ]; then \
			echo "    region-a is HEALTHY again (after $$((i*5))s)"; \
			break; \
		fi; \
		echo "    [$$((i*5))s] region-a healthy=$$HEALTHY, waiting..."; \
		sleep 5; \
	done
	@echo ""
	@echo "==> Waiting for redistribution (10s)..."
	@sleep 10
	@echo ""
	@echo "==> Restored placement:"
	kubectl get globalworkload hello-workload -o jsonpath='{.status.placements}' | jq
	@echo ""
	@echo "==> Deployments now in both clusters:"
	-kubectl --kubeconfig $(KUBECONFIG_DIR)/region-a.kubeconfig get deployment -n $(DEMO_NS) gwo-hello-workload
	-kubectl --kubeconfig $(KUBECONFIG_DIR)/region-b.kubeconfig get deployment -n $(DEMO_NS) gwo-hello-workload

.PHONY: demo-clean
demo-clean: ## Delete the demo workload (finalizer cleans target clusters)
	@echo "==> Deleting GlobalWorkload (finalizer will clean target Deployments)..."
	-kubectl delete globalworkload hello-workload --timeout=60s
	@echo ""
	@echo "==> Confirming Deployments are gone:"
	-kubectl --kubeconfig $(KUBECONFIG_DIR)/region-a.kubeconfig get deployment -n $(DEMO_NS) gwo-hello-workload 2>&1 | head -2
	-kubectl --kubeconfig $(KUBECONFIG_DIR)/region-b.kubeconfig get deployment -n $(DEMO_NS) gwo-hello-workload 2>&1 | head -2

.PHONY: demo-teardown
demo-teardown: ## Destroy all three kind clusters
	@echo "==> Deleting kind clusters..."
	kind delete cluster --name mgmt || true
	kind delete cluster --name region-a || true
	kind delete cluster --name region-b || true
	@echo "==> Teardown complete."
