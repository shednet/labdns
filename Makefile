# Image URL to use all building/pushing image targets
IMG ?= controller:dev

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Location to install Go-based build dependencies and envtest assets.
LOCALBIN ?= $(shell pwd)/bin
# Keep nested Go tool invocations on the toolchain selected from go.mod.
GO_TOOLCHAIN_ROOT := $(shell go env GOROOT)
export PATH := $(GO_TOOLCHAIN_ROOT)/bin:$(PATH)

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

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

.PHONY: lint-fmt
lint-fmt: ## Check Go source formatting without modifying it.
	@unformatted="$$(gofmt -l $$(find . -name '*.go' -not -path './bin/*'))"; \
	test -z "$$unformatted" || { echo "Go files require formatting:"; echo "$$unformatted"; exit 1; }

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

.PHONY: check-generated
check-generated: ## Check that generated artifacts are synchronized.
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	cp -a api config "$$tmp/"; \
	$(MAKE) --no-print-directory manifests generate; \
	diff -ru "$$tmp/api" api; \
	diff -ru "$$tmp/config" config

.PHONY: helm-sync
helm-sync: manifests ## Synchronize the chart's generated DNSProvider CRD.
	@mkdir -p charts/labdns/crds
	@cp config/crd/bases/labdns.shednet.dev_dnsproviders.yaml charts/labdns/crds/labdns.shednet.dev_dnsproviders.yaml

.PHONY: check-packaging
check-packaging: manifests kustomize ## Check Helm, Kustomize, examples, and install boundaries.
	cmp config/crd/bases/labdns.shednet.dev_dnsproviders.yaml charts/labdns/crds/labdns.shednet.dev_dnsproviders.yaml
	"$(HELM)" lint charts/labdns
	"$(HELM)" template labdns charts/labdns --namespace labdns-system >/dev/null
	"$(KUSTOMIZE)" build config/default >/dev/null
	"$(KUSTOMIZE)" build config/overlays/metrics >/dev/null
	"$(KUSTOMIZE)" build config/overlays/secure-metrics >/dev/null
	"$(KUSTOMIZE)" build config/overlays/gateway-api >/dev/null
	"$(KUSTOMIZE)" build examples >/dev/null

.PHONY: build-e2e
build-e2e: ## Vet and compile all packages with the E2E build tag.
	go vet -tags=e2e ./...
	go test -tags=e2e ./... -run '^$$'

.PHONY: lint-workflows
lint-workflows: actionlint ## Lint GitHub Actions workflow syntax and expressions.
	"$(ACTIONLINT)"

KIND_CLUSTER ?=
E2E_INVOCATION_ID ?=
KIND_EXPERIMENTAL_PROVIDER ?= docker
KIND_NODE_IMAGE ?= kindest/node:v1.35.0
KIND_CONFIG ?= test/e2e/kind.yaml
E2E_KEEP_CLUSTER_ON_FAILURE ?= false
E2E_DIAGNOSTICS_DIR ?=
export KIND_CLUSTER E2E_INVOCATION_ID KIND_EXPERIMENTAL_PROVIDER KIND_NODE_IMAGE KIND_CONFIG E2E_DIAGNOSTICS_DIR

.PHONY: setup-test-e2e
setup-test-e2e: ## Create a fresh isolated dual-stack Kind cluster for E2E tests.
	@test -n "$${KIND_CLUSTER}" && test -n "$${E2E_INVOCATION_ID}" || { \
		echo "KIND_CLUSTER and E2E_INVOCATION_ID must identify this isolated invocation." >&2; \
		exit 1; \
	}
	@[[ "$${E2E_INVOCATION_ID}" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$$ ]] || { \
		echo "E2E_INVOCATION_ID must be a path-safe identifier of at most 128 characters." >&2; \
		exit 1; \
	}
	@[[ "$${KIND_CLUSTER}" =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$$ ]] || { \
		echo "KIND_CLUSTER must be a lowercase Kind-compatible name of at most 63 characters." >&2; \
		exit 1; \
	}
	@test "$${KIND_EXPERIMENTAL_PROVIDER}" = "docker" || { \
		echo "E2E requires KIND_EXPERIMENTAL_PROVIDER=docker." >&2; \
		exit 1; \
	}
	@docker_identity="$$(docker info --format '{{.DockerRootDir}}|{{.OSType}}|{{.Architecture}}|{{.ServerVersion}}' 2>/dev/null)" || { \
		echo "E2E requires an available Docker Engine." >&2; \
		exit 1; \
	}; \
	IFS='|' read -r docker_root docker_os docker_arch docker_version docker_extra <<<"$$docker_identity"; \
	if [ -z "$$docker_root" ] || [ "$$docker_root" = '<no value>' ] || [ -z "$$docker_version" ] || [ "$$docker_version" = '<no value>' ] || [ -n "$$docker_extra" ]; then \
		echo "E2E requires Docker Engine identity fields DockerRootDir and ServerVersion." >&2; \
		exit 1; \
	fi
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is required and must be available on PATH." >&2; \
		exit 1; \
	}
	@clusters="$$( $(KIND) get clusters )" || { \
		echo "Unable to list Kind clusters; refusing E2E setup." >&2; \
		exit 1; \
	}; \
	if grep -Fxq -- "$${KIND_CLUSTER}" <<<"$$clusters"; then \
		echo "Refusing to reuse existing Kind cluster '$${KIND_CLUSTER}'. Choose a fresh KIND_CLUSTER name or clean it up explicitly." >&2; \
		exit 1; \
	fi
	@if [ -e "/tmp/labdns-kind-kubeconfig-$${E2E_INVOCATION_ID}" ]; then \
		echo "Refusing to overwrite the invocation kubeconfig '/tmp/labdns-kind-kubeconfig-$${E2E_INVOCATION_ID}'." >&2; \
		exit 1; \
	fi
	@if [ -e "/tmp/labdns-kind-owned-$${E2E_INVOCATION_ID}" ]; then \
		echo "Refusing to overwrite the invocation marker '/tmp/labdns-kind-owned-$${E2E_INVOCATION_ID}'." >&2; \
		exit 1; \
	fi
	@printf '%s\n%s\n' "$${E2E_INVOCATION_ID}" "$${KIND_CLUSTER}" >"/tmp/labdns-kind-owned-$${E2E_INVOCATION_ID}"
	@echo "Creating fresh dual-stack Kind cluster '$${KIND_CLUSTER}'..."
	@create_status=0; \
	$(KIND) create cluster --name "$${KIND_CLUSTER}" --image "$${KIND_NODE_IMAGE}" --config "$${KIND_CONFIG}" --kubeconfig "/tmp/labdns-kind-kubeconfig-$${E2E_INVOCATION_ID}" || create_status=$$?; \
	if [ $$create_status -ne 0 ]; then \
		echo "Kind creation failed; cleaning up any partially created cluster '$${KIND_CLUSTER}'." >&2; \
		$(MAKE) cleanup-test-e2e KIND_CLUSTER="$${KIND_CLUSTER}" E2E_INVOCATION_ID="$${E2E_INVOCATION_ID}" || true; \
		exit $$create_status; \
	fi

.PHONY: test-e2e
test-e2e: manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	@invocation_id="$${E2E_INVOCATION_ID}"; \
	if [ -z "$$invocation_id" ]; then invocation_id="$$(date -u +%Y%m%d%H%M%S)-$$$$-$${RANDOM}"; fi; \
	cluster_name="$${KIND_CLUSTER}"; \
	if [ -z "$$cluster_name" ]; then cluster_name="labdns-e2e-$$invocation_id"; fi; \
	diagnostics_dir="$${E2E_DIAGNOSTICS_DIR}"; \
	if [ -z "$$diagnostics_dir" ]; then diagnostics_dir="/tmp/labdns-kind-logs-$$invocation_id"; fi; \
	kubeconfig="/tmp/labdns-kind-kubeconfig-$$invocation_id"; \
	$(MAKE) setup-test-e2e KIND_CLUSTER="$$cluster_name" E2E_INVOCATION_ID="$$invocation_id"; \
	status=0; \
	KUBECONFIG="$$kubeconfig" KIND=$(KIND) KIND_CLUSTER="$$cluster_name" E2E_INVOCATION_ID="$$invocation_id" go test -tags=e2e ./test/e2e/ -v -ginkgo.v -timeout=8m || status=$$?; \
	if [ $$status -ne 0 ]; then \
		echo "E2E failed; exporting diagnostics to $$diagnostics_dir" >&2; \
		KIND="$(KIND)" KIND_CLUSTER="$$cluster_name" E2E_INVOCATION_ID="$$invocation_id" E2E_DIAGNOSTICS_DIR="$$diagnostics_dir" hack/collect-e2e-diagnostics.sh || true; \
		if [ "$(E2E_KEEP_CLUSTER_ON_FAILURE)" = "true" ]; then \
			echo "Preserving failed cluster for CI diagnostics; caller must delete $$cluster_name with invocation $$invocation_id." >&2; \
		else \
			$(MAKE) cleanup-test-e2e KIND_CLUSTER="$$cluster_name" E2E_INVOCATION_ID="$$invocation_id" || true; \
		fi; \
		exit $$status; \
	fi; \
	$(MAKE) cleanup-test-e2e KIND_CLUSTER="$$cluster_name" E2E_INVOCATION_ID="$$invocation_id"

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the exact Kind cluster used for e2e tests.
	@test -n "$${KIND_CLUSTER}" && test -n "$${E2E_INVOCATION_ID}" || { \
		echo "KIND_CLUSTER and E2E_INVOCATION_ID are required for cleanup." >&2; \
		exit 1; \
	}
	@[[ "$${E2E_INVOCATION_ID}" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$$ ]] || { \
		echo "E2E_INVOCATION_ID must be a path-safe identifier of at most 128 characters." >&2; \
		exit 1; \
	}
	@[[ "$${KIND_CLUSTER}" =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$$ ]] || { \
		echo "KIND_CLUSTER must be a lowercase Kind-compatible name of at most 63 characters." >&2; \
		exit 1; \
	}
	@clusters="$$( $(KIND) get clusters )" || { \
		echo "Unable to list Kind clusters; retaining the invocation marker and kubeconfig." >&2; \
		exit 1; \
	}; \
	if [ ! -f "/tmp/labdns-kind-owned-$${E2E_INVOCATION_ID}" ]; then \
		if grep -Fxq -- "$${KIND_CLUSTER}" <<<"$$clusters"; then \
			echo "Refusing to delete cluster '$${KIND_CLUSTER}' without its invocation marker." >&2; \
			exit 1; \
		fi; \
		echo "Kind cluster '$${KIND_CLUSTER}' is already absent."; \
		exit 0; \
	fi; \
	marker_invocation="$$(sed -n '1p' "/tmp/labdns-kind-owned-$${E2E_INVOCATION_ID}")"; \
	marker_cluster="$$(sed -n '2p' "/tmp/labdns-kind-owned-$${E2E_INVOCATION_ID}")"; \
	if [ "$$marker_invocation" != "$${E2E_INVOCATION_ID}" ] || [ "$$marker_cluster" != "$${KIND_CLUSTER}" ]; then \
		echo "Refusing cleanup: marker does not authorize invocation '$${E2E_INVOCATION_ID}' and cluster '$${KIND_CLUSTER}'." >&2; \
		exit 1; \
	fi; \
	if ! grep -Fxq -- "$${KIND_CLUSTER}" <<<"$$clusters"; then \
		rm -f "/tmp/labdns-kind-kubeconfig-$${E2E_INVOCATION_ID}" "/tmp/labdns-kind-owned-$${E2E_INVOCATION_ID}"; \
		echo "Kind cluster '$${KIND_CLUSTER}' is already absent."; \
		exit 0; \
	fi; \
	if ! $(KIND) delete cluster --name "$${KIND_CLUSTER}" --kubeconfig "/tmp/labdns-kind-kubeconfig-$${E2E_INVOCATION_ID}"; then \
		echo "Kind cleanup failed; retaining the invocation marker and kubeconfig." >&2; \
		exit 1; \
	fi; \
	rm -f "/tmp/labdns-kind-kubeconfig-$${E2E_INVOCATION_ID}" "/tmp/labdns-kind-owned-$${E2E_INVOCATION_ID}"

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Check golangci-lint configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager ./cmd

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name labdns-builder
	$(CONTAINER_TOOL) buildx use labdns-builder
	$(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm labdns-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	KUSTOMIZE="$(KUSTOMIZE)" hack/render-kustomize.sh "${IMG}" > dist/install.yaml

.PHONY: helm-package
helm-package: helm-sync ## Package the labdns Helm chart.
	mkdir -p dist
	"$(HELM)" lint charts/labdns
	"$(HELM)" package charts/labdns --destination dist

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	KUSTOMIZE="$(KUSTOMIZE)" hack/render-kustomize.sh "${IMG}" | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
ACTIONLINT ?= $(LOCALBIN)/actionlint
HELM ?= helm

## Tool Versions
KUSTOMIZE_VERSION ?= v5.7.1
CONTROLLER_TOOLS_VERSION ?= v0.20.0

#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= v0.0.0-20260305142021-f9589b9f2b9d

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= 1.35.0

GOLANGCI_LINT_VERSION ?= v2.7.2
ACTIONLINT_VERSION ?= v1.7.7

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
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: actionlint
actionlint: $(ACTIONLINT) ## Download the pinned GitHub Actions workflow linter locally.
$(ACTIONLINT): $(LOCALBIN)
	$(call go-install-tool,$(ACTIONLINT),github.com/rhysd/actionlint/cmd/actionlint,$(ACTIONLINT_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
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
