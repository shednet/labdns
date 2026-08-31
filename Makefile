# Image URL to use all building/pushing image targets
IMG ?= controller:dev

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Keep all downloaded toolchain state inside this greenfield repository. This
# avoids accidentally reusing tools or envtest assets from the legacy project.
LOCALBIN ?= $(shell pwd)/bin
GOCACHE ?= /tmp/labdns-next-go-build
GOMODCACHE ?= /tmp/labdns-next-go-mod
GOLANGCI_LINT_CACHE ?= /tmp/labdns-next-golangci-lint
export GOCACHE GOMODCACHE GOLANGCI_LINT_CACHE
GO_TOOLCHAIN_VERSION ?= go1.26.1
GO_TOOLCHAIN_ROOT := $(shell GOTOOLCHAIN=$(GO_TOOLCHAIN_VERSION) go env GOROOT)
# The auto-downloaded toolchain can invoke `go tool` internally. Put its bin
# directory first so those nested calls cannot fall back to an older host Go.
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

.PHONY: verify-fmt
verify-fmt: ## Verify that Go source is formatted without modifying it.
	@test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './bin/*'))" || { \
		echo "Go files require formatting:"; \
		gofmt -l $$(find . -name '*.go' -not -path './bin/*'); \
		exit 1; \
	}

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate verify-fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

.PHONY: verify-generated
verify-generated: ## Verify generated artifacts are synchronized.
	@hack/verify-generated.sh

.PHONY: verify-artifacts
verify-artifacts: manifests kustomize ## Verify the Kustomize deployment renders.
	@hack/verify-artifacts.sh

.PHONY: helm-sync
helm-sync: manifests ## Synchronize the chart's generated DNSProvider CRD.
	@mkdir -p charts/labdns/crds
	@cp config/crd/bases/labdns.shednet.dev_dnsproviders.yaml charts/labdns/crds/labdns.shednet.dev_dnsproviders.yaml

.PHONY: verify-packaging
verify-packaging: manifests kustomize helm ## Verify Helm, Kustomize, examples, and install boundaries.
	@HELM="$(HELM)" KUSTOMIZE="$(KUSTOMIZE)" hack/verify-packaging.sh

.PHONY: verify-e2e-build
verify-e2e-build: verify-e2e-safety verify-e2e-contract ## Vet and compile all packages with the E2E build tag.
	go vet -tags=e2e ./...
	go test -tags=e2e ./... -run '^$$'

.PHONY: verify-e2e-safety
verify-e2e-safety: ## Verify isolated Kind naming, failure diagnostics, and guarded cleanup.
	@hack/verify-e2e-safety.sh

.PHONY: verify-e2e-contract
verify-e2e-contract: ## Verify the pinned, isolated, exact E2E publication contract.
	@hack/verify-e2e-contract.sh

.PHONY: verify-workflows
verify-workflows: actionlint ## Validate GitHub Actions workflow syntax and expressions.
	"$(ACTIONLINT)"

KIND_CLUSTER ?=
E2E_INVOCATION_ID ?=
KIND_EXPERIMENTAL_PROVIDER ?= docker
KIND_NODE_IMAGE ?= kindest/node:v1.35.0
KIND_CONFIG ?= test/e2e/kind.yaml
E2E_KEEP_CLUSTER_ON_FAILURE ?= false
E2E_DIAGNOSTICS_DIR ?=
E2E_SUITE_GLOB ?= test/e2e/*_test.go
E2E_TEST_COMMAND ?= go test -tags=e2e ./test/e2e/ -v -ginkgo.v -timeout=8m
export KIND_CLUSTER E2E_INVOCATION_ID KIND_EXPERIMENTAL_PROVIDER KIND_NODE_IMAGE KIND_CONFIG E2E_DIAGNOSTICS_DIR

.PHONY: setup-test-e2e
setup-test-e2e: kind ## Create a fresh isolated dual-stack Kind cluster for E2E tests.
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
		echo "Kind is required; CI uses pinned Kind v0.30.0." >&2; \
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
	@compgen -G '$(E2E_SUITE_GLOB)' >/dev/null || { echo "Stage 5 E2E suite is not present" >&2; exit 1; }
	@invocation_id="$${E2E_INVOCATION_ID}"; \
	if [ -z "$$invocation_id" ]; then invocation_id="$$(date -u +%Y%m%d%H%M%S)-$$$$-$${RANDOM}"; fi; \
	cluster_name="$${KIND_CLUSTER}"; \
	if [ -z "$$cluster_name" ]; then cluster_name="labdns-e2e-$$invocation_id"; fi; \
	diagnostics_dir="$${E2E_DIAGNOSTICS_DIR}"; \
	if [ -z "$$diagnostics_dir" ]; then diagnostics_dir="/tmp/labdns-kind-logs-$$invocation_id"; fi; \
	kubeconfig="/tmp/labdns-kind-kubeconfig-$$invocation_id"; \
	$(MAKE) setup-test-e2e KIND_CLUSTER="$$cluster_name" E2E_INVOCATION_ID="$$invocation_id"; \
	status=0; \
	KUBECONFIG="$$kubeconfig" KIND=$(KIND) KIND_CLUSTER="$$cluster_name" E2E_INVOCATION_ID="$$invocation_id" $(E2E_TEST_COMMAND) || status=$$?; \
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
cleanup-test-e2e: kind ## Tear down the exact Kind cluster used for e2e tests.
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
lint-config: golangci-lint ## Verify golangci-lint linter configuration
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
helm-package: helm-sync helm ## Package the labdns Helm chart.
	mkdir -p dist
	"$(HELM)" dependency build charts/labdns
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
KIND ?= $(LOCALBIN)/kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
ACTIONLINT ?= $(LOCALBIN)/actionlint
HELM ?= $(LOCALBIN)/helm
KUBEBUILDER ?= $(LOCALBIN)/kubebuilder
JUST ?= $(LOCALBIN)/just

## Tool Versions
KUSTOMIZE_VERSION ?= v5.7.1
CONTROLLER_TOOLS_VERSION ?= v0.20.0
KUBEBUILDER_VERSION ?= v4.11.1

#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= v0.0.0-20260305142021-f9589b9f2b9d

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= 1.35.0

GOLANGCI_LINT_VERSION ?= v2.7.2
ACTIONLINT_VERSION ?= v1.7.7
HELM_VERSION ?= v3.20.2
KIND_VERSION ?= v0.30.0
JUST_VERSION ?= 1.51.0

.PHONY: verify-justfile
verify-justfile: just ## Verify the justfile release workflow without modifying this repository.
	@JUST="$(JUST)" hack/verify-justfile.sh

.PHONY: kubebuilder
kubebuilder: $(KUBEBUILDER) ## Download the pinned Kubebuilder CLI locally.
$(KUBEBUILDER): $(LOCALBIN)/kubebuilder-$(KUBEBUILDER_VERSION)
	ln -sf "$$(realpath "$<")" "$@"
$(LOCALBIN)/kubebuilder-$(KUBEBUILDER_VERSION): | $(LOCALBIN)
	@set -e; \
	os="$$(go env GOOS)"; arch="$$(go env GOARCH)"; \
	case "$${os}/$${arch}" in \
		darwin/amd64) checksum="20afe0a4e11e44515a03d9bb7230e8f044190bb9a16d7a1cddcd8c10d19a0f3b" ;; \
		darwin/arm64) checksum="501372d81715661049ea162343138aa9f601b3aeb50fbeb594278292650c76f4" ;; \
		linux/amd64) checksum="834d26c233881ee1f0bb73a7fdcfa3ef8b264892827c50ee51a7653fac70e4f6" ;; \
		linux/arm64) checksum="cba576bd94cb1f49049d585732245b89b70d9622923f6492fa45a94720d0d781" ;; \
		linux/ppc64le) checksum="bd4061a399cd524a5097e9fc4ae9f68b13bac83a41fa91e28e653d5612cdfaa9" ;; \
		linux/s390x) checksum="884f268ff3b0cd9cb9107a8de2512becbd63d03cafac9293fcc271415819ea6e" ;; \
		*) echo "No Kubebuilder $(KUBEBUILDER_VERSION) checksum is pinned for $${os}/$${arch}" >&2; exit 1 ;; \
	esac; \
	url="https://github.com/kubernetes-sigs/kubebuilder/releases/download/$(KUBEBUILDER_VERSION)/kubebuilder_$${os}_$${arch}"; \
	echo "Downloading $${url}"; \
	curl -fL "$${url}" -o "$@.tmp"; \
	if command -v sha256sum >/dev/null 2>&1; then \
		actual="$$(sha256sum "$@.tmp" | awk '{print $$1}')"; \
	elif command -v shasum >/dev/null 2>&1; then \
		actual="$$(shasum -a 256 "$@.tmp" | awk '{print $$1}')"; \
	else \
		echo "A SHA-256 utility (sha256sum or shasum) is required" >&2; \
		rm -f "$@.tmp"; \
		exit 1; \
	fi; \
	[ "$${actual}" = "$${checksum}" ] || { \
		echo "Kubebuilder $(KUBEBUILDER_VERSION) checksum verification failed" >&2; \
		rm -f "$@.tmp"; \
		exit 1; \
	}; \
	chmod 0755 "$@.tmp"; \
	mv "$@.tmp" "$@"
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

.PHONY: helm
helm: $(HELM) ## Download the pinned Helm CLI locally.
$(HELM): $(LOCALBIN)/helm-$(HELM_VERSION)
	ln -sf "$$(realpath "$<")" "$@"
$(LOCALBIN)/helm-$(HELM_VERSION): | $(LOCALBIN)
	@set -e; \
	os="$$(go env GOOS)"; arch="$$(go env GOARCH)"; \
	case "$${os}/$${arch}" in \
		linux/amd64) checksum="258e830a9e613c8a7a302d6059b4bb3b9758f2f3e1bb8ea0d707ce10a9a72fea" ;; \
		linux/arm64) checksum="5ea2d6bc2cda3f8edf985e028809f5a9278f404fb8ab24044de9b7cb9b79a691" ;; \
		darwin/amd64) checksum="7de04301f28b902a74f6286ed941cadc86ee5e6a9086a18f2ccf1f548e99d618" ;; \
		darwin/arm64) checksum="139c794c22f16b579d08ddd3008c8038b9bb2814f35b5bcca91f50a1f458978d" ;; \
		*) echo "No Helm $(HELM_VERSION) checksum is pinned for $${os}/$${arch}" >&2; exit 1 ;; \
	esac; \
	archive="$$(mktemp)"; extract="$$(mktemp -d)"; \
	trap 'rm -f "$${archive}"; rm -rf "$${extract}"' EXIT; \
	curl -fL "https://get.helm.sh/helm-$(HELM_VERSION)-$${os}-$${arch}.tar.gz" -o "$${archive}"; \
	if command -v sha256sum >/dev/null 2>&1; then \
		actual="$$(sha256sum "$${archive}" | awk '{print $$1}')"; \
	else \
		actual="$$(shasum -a 256 "$${archive}" | awk '{print $$1}')"; \
	fi; \
	[ "$${actual}" = "$${checksum}" ] || { echo "Helm checksum verification failed" >&2; exit 1; }; \
	tar -xzf "$${archive}" -C "$${extract}"; \
	install -m 0755 "$${extract}/$${os}-$${arch}/helm" "$@"

.PHONY: kind
kind: $(KIND) ## Download the pinned Kind CLI locally.
# Maintain only the repository-default link. A caller-provided KIND is an
# external executable and must never be replaced by this tool-install rule.
$(LOCALBIN)/kind: $(LOCALBIN)/kind-$(KIND_VERSION)
	ln -sf "$$(realpath "$<")" "$@"
$(LOCALBIN)/kind-$(KIND_VERSION): | $(LOCALBIN)
	@set -e; \
	os="$$(go env GOOS)"; arch="$$(go env GOARCH)"; \
	case "$${os}/$${arch}" in \
		linux/amd64) checksum="517ab7fc89ddeed5fa65abf71530d90648d9638ef0c4cde22c2c11f8097b8889" ;; \
		linux/arm64) checksum="7ea2de9d2d190022ed4a8a4e3ac0636c8a455e460b9a13ccf19f15d07f4f00eb" ;; \
		darwin/amd64) checksum="4f0b6e3b88bdc66d922c08469f05ef507d4903dd236e6319199bb9c868eed274" ;; \
		darwin/arm64) checksum="ceaf40df1d1551c481fb50e3deb5c3deecad5fd599df5469626b70ddf52a1518" ;; \
		*) echo "No Kind $(KIND_VERSION) checksum is pinned for $${os}/$${arch}" >&2; exit 1 ;; \
	esac; \
	temporary="$@.tmp"; \
	trap 'rm -f "$${temporary}"' EXIT; \
	curl -fL "https://github.com/kubernetes-sigs/kind/releases/download/$(KIND_VERSION)/kind-$${os}-$${arch}" -o "$${temporary}"; \
	if command -v sha256sum >/dev/null 2>&1; then \
		actual="$$(sha256sum "$${temporary}" | awk '{print $$1}')"; \
	else \
		actual="$$(shasum -a 256 "$${temporary}" | awk '{print $$1}')"; \
	fi; \
	[ "$${actual}" = "$${checksum}" ] || { echo "Kind checksum verification failed" >&2; exit 1; }; \
	chmod 0755 "$${temporary}"; \
	mv "$${temporary}" "$@"

.PHONY: just
just: $(JUST) ## Download the pinned just command runner locally.
$(LOCALBIN)/just: $(LOCALBIN)/just-$(JUST_VERSION)
	ln -sf "$$(realpath "$<")" "$@"
$(LOCALBIN)/just-$(JUST_VERSION): | $(LOCALBIN)
	@set -e; \
	os="$$(go env GOOS)"; arch="$$(go env GOARCH)"; \
	case "$${os}/$${arch}" in \
		linux/amd64) target="x86_64-unknown-linux-musl"; checksum="c8f085ca3e885723c341d06243fc291b5abfdc8bbe3b2c076b117de490387b59" ;; \
		linux/arm64) target="aarch64-unknown-linux-musl"; checksum="ed7ec466b77709198fd4afed253dba0270203ba5eb1c006bee2b0139090284f5" ;; \
		darwin/amd64) target="x86_64-apple-darwin"; checksum="d583e45f1f9fcdd26069ad2fe3bb9dea414756d8d0752eb9093974cb5c0246f0" ;; \
		darwin/arm64) target="aarch64-apple-darwin"; checksum="61e3f1b8a545ff064b091eab4b6e14f8cc743ff15549be293b1e92f5b1467002" ;; \
		*) echo "No just $(JUST_VERSION) checksum is pinned for $${os}/$${arch}" >&2; exit 1 ;; \
	esac; \
	archive="$$(mktemp)"; extract="$$(mktemp -d)"; \
	trap 'rm -f "$${archive}"; rm -rf "$${extract}"' EXIT; \
	url="https://github.com/casey/just/releases/download/$(JUST_VERSION)/just-$(JUST_VERSION)-$${target}.tar.gz"; \
	curl -fL "$${url}" -o "$${archive}"; \
	if command -v sha256sum >/dev/null 2>&1; then \
		actual="$$(sha256sum "$${archive}" | awk '{print $$1}')"; \
	else \
		actual="$$(shasum -a 256 "$${archive}" | awk '{print $$1}')"; \
	fi; \
	[ "$${actual}" = "$${checksum}" ] || { echo "just checksum verification failed" >&2; exit 1; }; \
	tar -xzf "$${archive}" -C "$${extract}"; \
	install -m 0755 "$${extract}/just" "$@"

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
