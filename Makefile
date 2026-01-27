export PROJECT_NAME=kubernetes-power-manager
KPM_NAMESPACE ?= intel-power
# Current Operator version
VERSION ?= 0.0.1
# Bundle version (without 'v' prefix for operator-sdk)
BUNDLE_VERSION := $(shell echo $(VERSION) | sed 's/^v//')
# parameter used for helm chart image
HELM_CHART ?= v2.5.0
HELM_VERSION := $(shell echo $(HELM_CHART) | cut -d "v" -f2)

# CONTROLLER_GEN_VERSION defines the controller-gen version to download from go modules.
CONTROLLER_GEN_VERSION ?= v0.18.0

# KUSTOMIZE_VERSION defines the kustomize version to download from go modules.
KUSTOMIZE_VERSION ?= v5@v5.7.1

# OPERATOR_SDK_VERSION defines the operator-sdk version to download from GitHub releases.
OPERATOR_SDK_VERSION ?= 1.42.0

# OPM_VERSION defines the opm version to download from GitHub releases.
OPM_VERSION ?= v1.52.0

# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION ?= 1.32.0
ENVTEST_VERSION ?= release-0.21

# used to detemine if certain targets should build for openshift
OCP ?= false

IMAGE_REGISTRY ?= quay.io/openshift-kni

IMAGE_NAME ?= $(PROJECT_NAME)-operator
IMAGE_NAME_AGENT ?= kubernetes-power-node-agent
IMAGE_TAG_BASE ?= $(IMAGE_REGISTRY)/$(IMAGE_NAME)
IMAGE_TAG_BASE_AGENT ?= $(IMAGE_REGISTRY)/$(IMAGE_NAME_AGENT)

# Image URL to use all building/pushing image targets
IMG ?= $(IMAGE_TAG_BASE):$(VERSION)
IMG_AGENT ?= $(IMAGE_TAG_BASE_AGENT):$(VERSION)

# Default bundle image tag
BUNDLE_IMG ?= $(IMAGE_TAG_BASE)-bundle:$(VERSION)
# BUNDLE_GEN_FLAGS are the flags passed to the operator-sdk generate bundle command
BUNDLE_GEN_FLAGS ?= -q --overwrite --version $(BUNDLE_VERSION) $(BUNDLE_METADATA_OPTS)

# version of ocp being supported
OCP_VERSION=4.22
# image used for building the dockerfile for ocp
OCP_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal:9.5-1742914212
# Platform to build the images for.
PLATFORM ?= linux/amd64
# Multi-arch platforms to build for.
PLATFORMS ?= linux/amd64,linux/arm64
# ARM64 variant for multi-arch builds
ARM64_VARIANT ?= v8
# Target architecture.
GOARCH = $(shell go env GOARCH)

## Location to install dependencies.
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= $(LOCALBIN)/kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
OPERATOR_SDK ?= $(LOCALBIN)/operator-sdk
OPM ?= $(LOCALBIN)/opm

.PHONY: kubectl
kubectl: $(KUBECTL) ## Use envtest to download kubectl
$(KUBECTL): $(LOCALBIN) envtest
	@if [ ! -f $(KUBECTL) ] || ! $(KUBECTL) version 2>/dev/null | grep -q "Client Version: v$(ENVTEST_K8S_VERSION)$$"; then \
		KUBEBUILDER_ASSETS=$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path); \
		ln -sf $${KUBEBUILDER_ASSETS}/kubectl $(KUBECTL); \
	fi

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary. If wrong version is installed, it will be removed before downloading.
$(KUSTOMIZE): $(LOCALBIN)
	@if test -x $(KUSTOMIZE) && ! $(KUSTOMIZE) version 2>/dev/null | grep -q "$(KUSTOMIZE_VERSION)"; then \
		echo "$(KUSTOMIZE) version is not expected $(KUSTOMIZE_VERSION). Removing it before installing."; \
		rm -rf $(KUSTOMIZE); \
	fi
	@if [ ! -f $(KUSTOMIZE) ]; then \
		echo "Downloading kustomize..." ;\
		GOBIN=$(LOCALBIN) GO111MODULE=on go install sigs.k8s.io/kustomize/kustomize/$(KUSTOMIZE_VERSION) ;\
		echo "kustomize downloaded successfully." ;\
	fi

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary. If wrong version is installed, it will be removed before downloading.
$(CONTROLLER_GEN): $(LOCALBIN)
	@if test -x $(CONTROLLER_GEN) && ! $(CONTROLLER_GEN) --version 2>/dev/null | grep -q "$(CONTROLLER_GEN_VERSION)"; then \
		echo "$(CONTROLLER_GEN) version is not expected $(CONTROLLER_GEN_VERSION). Removing it before installing."; \
		rm -rf $(CONTROLLER_GEN); \
	fi
	@if [ ! -f $(CONTROLLER_GEN) ]; then \
		echo "Downloading controller-gen..." ;\
		GOBIN=$(LOCALBIN) GOFLAGS="-mod=mod" go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) ;\
		echo "controller-gen downloaded successfully." ;\
	fi

.PHONY: envtest
envtest: $(ENVTEST) ## Download envtest-setup locally if necessary.
$(ENVTEST): $(LOCALBIN)
	@if test -x $(ENVTEST) && ! $(ENVTEST) version 2>/dev/null | grep -q "$(ENVTEST_VERSION)"; then \
		echo "$(ENVTEST) version is not expected $(ENVTEST_VERSION). Removing it before installing."; \
		rm -rf $(ENVTEST); \
	fi
	@if [ ! -f $(ENVTEST) ]; then \
		echo "Downloading setup-envtest..." ;\
		GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION) ;\
		echo "setup-envtest downloaded successfully." ;\
	fi

# Options for 'bundle-build'
ifneq ($(origin CHANNELS), undefined)
BUNDLE_CHANNELS := --channels=$(CHANNELS)
endif
ifneq ($(origin DEFAULT_CHANNEL), undefined)
BUNDLE_DEFAULT_CHANNEL := --default-channel=$(DEFAULT_CHANNEL)
endif
BUNDLE_METADATA_OPTS ?= $(BUNDLE_CHANNELS) $(BUNDLE_DEFAULT_CHANNEL)

IMGTOOL ?= docker
CATALOG_IMG ?= $(IMAGE_TAG_BASE)-catalog:v$(VERSION)

TLS_VERIFY ?= false

# The image tag given to the resulting catalog image (e.g. make catalog-build CATALOG_IMG=example.com/operator-catalog:v0.2.0).
CATALOG_IMG ?= $(IMAGE_TAG_BASE)-catalog:v$(VERSION)

# Set CATALOG_BASE_IMG to an existing catalog image tag to add $BUNDLE_IMGS to that image.
ifneq ($(origin CATALOG_BASE_IMG), undefined)
FROM_INDEX_OPT := --from-index $(CATALOG_BASE_IMG)
endif

BUNDLE_IMGS ?= $(BUNDLE_IMG)

# Produce CRDs that work back to Kubernetes 1.11 (no version conversion)
CRD_OPTIONS ?= "crd:crdVersions=v1"
# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

.PHONY: all test build images images-ocp run

all: manifests generate install

# Run tests
ENVTEST_ASSETS_DIR = $(shell pwd)/testbin
test: generate fmt vet manifests
	go test -v ./... -coverprofile cover.out
	cd power-optimization-library && go test -v ./... -coverprofile cover.out

# Build manager binary
build: generate manifests
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) GO111MODULE=on go build -a -o build/bin/manager build/manager/main.go
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) GO111MODULE=on go build -a -o build/bin/nodeagent build/nodeagent/main.go

verify-build: gofmt test race coverage tidy clean verify-test
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) GO111MODULE=on go build -a -o build/bin/manager build/manager/main.go
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) GO111MODULE=on go build -a -o build/bin/nodeagent build/nodeagent/main.go	

# Build the Manager and Node Agent images
images: generate manifests
	 $(IMGTOOL) build -f build/Dockerfile --platform $(PLATFORM) -t ${IMG} .
	 $(IMGTOOL) build -f build/Dockerfile.nodeagent --platform $(PLATFORM) -t ${IMG_AGENT} .

images-ocp: generate manifests
	 echo "Building images for OCP $(IMG) and $(IMG_AGENT)"
	 $(IMGTOOL) build --build-arg="BASE_IMAGE=$(OCP_IMAGE)" --build-arg="MANIFEST=build/manifests/ocp/power-node-agent-ds.yaml" -f build/Dockerfile --platform $(PLATFORM) -t ${IMG} .
	 $(IMGTOOL) build --build-arg="BASE_IMAGE=$(OCP_IMAGE)" -f build/Dockerfile.nodeagent --platform $(PLATFORM) -t ${IMG_AGENT} .
# Run against the configured Kubernetes cluster in ~/.kube/config
run: generate fmt vet manifests
	go run ./build/manager/main.go

run-agent: generate fmt vet manifests
	go run ./build/nodeagent/main.go

.PHONY: helm-install helm-uninstall 
helm-install:
ifeq (true, $(OCP))
	$(eval HELM_FLAG:=--set ocp=true)
	$(eval OCP_SUFFIX:=_ocp-$(OCP_VERSION))
endif
	sed -i 's/^version:.*$$/version: $(HELM_VERSION)/' helm/kubernetes-power-manager/Chart.yaml 
	sed -i 's/^appVersion:.*$$/appVersion: \"$(HELM_CHART)\"/' helm/kubernetes-power-manager/Chart.yaml
	sed -i 's/^version:.*$$/version: $(HELM_VERSION)/' helm/crds/Chart.yaml 
	sed -i 's/^appVersion:.*$$/appVersion: \"$(HELM_CHART)\"/' helm/crds/Chart.yaml 
	helm install kubernetes-power-manager-crds ./helm/crds
	helm dependency update ./helm/kubernetes-power-manager
	helm install kubernetes-power-manager-$(HELM_CHART) ./helm/kubernetes-power-manager --set operator.container.image=intel/power-operator$(OCP_SUFFIX):$(HELM_CHART) $(HELM_FLAG)

helm-uninstall:
	sed -i 's/^version:.*$$/version: $(HELM_VERSION)/' helm/kubernetes-power-manager/Chart.yaml 
	sed -i 's/^appVersion:.*$$/appVersion: \"$(HELM_CHART)\"/' helm/kubernetes-power-manager/Chart.yaml 
	sed -i 's/^version:.*$$/version: $(HELM_VERSION)/' helm/crds/Chart.yaml 
	sed -i 's/^appVersion:.*$$/appVersion: \"$(HELM_CHART)\"/' helm/crds/Chart.yaml 
	helm uninstall kubernetes-power-manager-$(HELM_CHART)
	helm uninstall kubernetes-power-manager-crds

.PHONY: install uninstall deploy manifests fmt vet tls
# Install CRDs into a cluster
install: manifests kustomize
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

# Uninstall CRDs from a cluster
uninstall: manifests kustomize
	$(KUSTOMIZE) build config/crd | kubectl delete -f -

# Deploy controller in the configured Kubernetes cluster in ~/.kube/config
deploy: manifests kustomize
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | kubectl apply -f -

# Undeploy controller from the K8s cluster specified in ~/.kube/config.
undeploy:
	$(KUSTOMIZE) build config/default | kubectl delete -f -

# Generate manifests e.g. CRD, RBAC etc.
manifests: controller-gen
ifeq (false, $(OCP))
	sed -i 's/- .*\/rbac\.yaml/- \.\/rbac.yaml/' config/rbac/kustomization.yaml
	sed -i 's/- .*\/role\.yaml/- \.\/role.yaml/' config/rbac/kustomization.yaml
else
	sed -i 's/- .*\/rbac\.yaml/- \.\/ocp\/rbac.yaml/' config/rbac/kustomization.yaml
	sed -i 's/- .*\/role\.yaml/- \.\/ocp\/role.yaml/' config/rbac/kustomization.yaml
endif
	$(CONTROLLER_GEN) $(CRD_OPTIONS) rbac:roleName=manager-role webhook paths="./..." output:crd:artifacts:config=config/crd/bases

# Run go fmt against code
fmt:
	go fmt ./...

# Run go vet against code
vet:
	go vet -composites=false ./...

# Testing the generation of TLS certificates
tls:
	./build/gen_test_certs.sh

.PHONY: generate build-controller build-agent build-controller-ocp build-agent-ocp
# Generate code
generate: controller-gen
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..." paths="./controllers/..."

# Build the Manager's image
build-controller:
	$(IMGTOOL) build -f build/Dockerfile --platform $(PLATFORM) -t ${IMG} .

# Build the Node Agent's image
build-agent:
	$(IMGTOOL) build -f build/Dockerfile.nodeagent --platform $(PLATFORM) -t ${IMG_AGENT} .

build-controller-ocp:
	$(IMGTOOL) build --build-arg="BASE_IMAGE=$(OCP_IMAGE)" -f build/Dockerfile --platform $(PLATFORM) -t ${IMG} .

build-agent-ocp:
	$(IMGTOOL) build --build-arg="BASE_IMAGE=$(OCP_IMAGE)" -f build/Dockerfile.nodeagent --platform $(PLATFORM) -t ${IMG_AGENT} .

.PHONY: build-push-multiarch
# Build and push multi-architecture images for both operator and agent
# Set OCP=true for OpenShift builds (default: false)
build-push-multiarch: generate manifests
ifeq (true, $(OCP))
	@echo "Building and pushing multi-arch OCP images for platforms: $(PLATFORMS)"
else
	@echo "Building and pushing multi-arch images for platforms: $(PLATFORMS)"
endif
	@echo "Operator: $(IMG)"
	@echo "Agent: $(IMG_AGENT)"
ifeq ($(IMGTOOL),podman)
	# Podman: build for each platform, create manifest, and push
ifeq (true, $(OCP))
	@for platform in $$(echo $(PLATFORMS) | tr ',' ' '); do \
		arch=$$(echo $$platform | cut -d'/' -f2); \
		echo "Building OCP operator for $$platform..."; \
		$(IMGTOOL) build --build-arg="BASE_IMAGE=$(OCP_IMAGE)" --build-arg="MANIFEST=build/manifests/ocp/power-node-agent-ds.yaml" -f build/Dockerfile --platform $$platform -t ${IMG}-$$arch .; \
		echo "Building OCP agent for $$platform..."; \
		$(IMGTOOL) build --build-arg="BASE_IMAGE=$(OCP_IMAGE)" -f build/Dockerfile.nodeagent --platform $$platform -t ${IMG_AGENT}-$$arch .; \
	done
else
	@for platform in $$(echo $(PLATFORMS) | tr ',' ' '); do \
		arch=$$(echo $$platform | cut -d'/' -f2); \
		echo "Building operator for $$platform..."; \
		$(IMGTOOL) build -f build/Dockerfile --platform $$platform -t ${IMG}-$$arch .; \
		echo "Building agent for $$platform..."; \
		$(IMGTOOL) build -f build/Dockerfile.nodeagent --platform $$platform -t ${IMG_AGENT}-$$arch .; \
	done
endif
	@echo "Creating multi-arch manifests..."
	$(IMGTOOL) manifest exists ${IMG} && $(IMGTOOL) manifest rm ${IMG} || true
	$(IMGTOOL) rmi ${IMG} 2>/dev/null || true
	$(IMGTOOL) manifest create ${IMG}
	@for platform in $$(echo $(PLATFORMS) | tr ',' ' '); do \
		arch=$$(echo $$platform | cut -d'/' -f2); \
		if [ "$$arch" = "arm64" ]; then \
			$(IMGTOOL) manifest add --arch $$arch --variant $(ARM64_VARIANT) ${IMG} ${IMG}-$$arch; \
		else \
			$(IMGTOOL) manifest add --arch $$arch --variant "" ${IMG} ${IMG}-$$arch; \
		fi; \
	done
	$(IMGTOOL) manifest exists ${IMG_AGENT} && $(IMGTOOL) manifest rm ${IMG_AGENT} || true
	$(IMGTOOL) rmi ${IMG_AGENT} 2>/dev/null || true
	$(IMGTOOL) manifest create ${IMG_AGENT}
	@for platform in $$(echo $(PLATFORMS) | tr ',' ' '); do \
		arch=$$(echo $$platform | cut -d'/' -f2); \
		if [ "$$arch" = "arm64" ]; then \
			$(IMGTOOL) manifest add --arch $$arch --variant $(ARM64_VARIANT) ${IMG_AGENT} ${IMG_AGENT}-$$arch; \
		else \
			$(IMGTOOL) manifest add --arch $$arch --variant "" ${IMG_AGENT} ${IMG_AGENT}-$$arch; \
		fi; \
	done
	@echo "Pushing multi-arch manifests..."
	$(IMGTOOL) manifest push ${IMG}
	$(IMGTOOL) manifest push ${IMG_AGENT}
	@echo "Cleaning up architecture-specific tags..."
	@for platform in $$(echo $(PLATFORMS) | tr ',' ' '); do \
		arch=$$(echo $$platform | cut -d'/' -f2); \
		$(IMGTOOL) rmi ${IMG}-$$arch 2>/dev/null || true; \
		$(IMGTOOL) rmi ${IMG_AGENT}-$$arch 2>/dev/null || true; \
	done
else
	# Docker: use buildx for multi-platform builds and push directly
ifeq (true, $(OCP))
	@echo "Using docker buildx for multi-platform OCP builds..."
	$(IMGTOOL) buildx build --build-arg="BASE_IMAGE=$(OCP_IMAGE)" --build-arg="MANIFEST=build/manifests/ocp/power-node-agent-ds.yaml" -f build/Dockerfile --platform $(PLATFORMS) -t ${IMG} --push .
	$(IMGTOOL) buildx build --build-arg="BASE_IMAGE=$(OCP_IMAGE)" -f build/Dockerfile.nodeagent --platform $(PLATFORMS) -t ${IMG_AGENT} --push .
else
	@echo "Using docker buildx for multi-platform builds..."
	$(IMGTOOL) buildx build -f build/Dockerfile --platform $(PLATFORMS) -t ${IMG} --push .
	$(IMGTOOL) buildx build -f build/Dockerfile.nodeagent --platform $(PLATFORMS) -t ${IMG_AGENT} --push .
endif
endif
	@echo "Multi-arch images built and pushed successfully"

.PHONY: docker-push controller-gen kustomize bundle bundle-build bundle-push
# Push the image
push:
	$(IMGTOOL) push ${IMG}

# Generate bundle manifests and metadata, then validate generated files.
bundle: update manifests kustomize operator-sdk
# directory used to get image name for bundle
ifeq (false, $(OCP))
	sed -i 's|^\- \.\./manager/ocp$$|- ../manager|' config/default/kustomization.yaml
else
	sed -i 's|^\- \.\./manager$$|- ../manager/ocp|' config/default/kustomization.yaml
endif
	$(OPERATOR_SDK) generate kustomize manifests -q
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/manifests | $(OPERATOR_SDK) generate bundle -q --use-image-digests --overwrite --version $(BUNDLE_VERSION) $(BUNDLE_METADATA_OPTS)
	$(OPERATOR_SDK) bundle validate ./bundle

# Build the bundle image.
bundle-build:
	$(IMGTOOL) build -f bundle.Dockerfile -t $(BUNDLE_IMG) .
# Push bundle image
bundle-push:
	$(IMGTOOL) push $(BUNDLE_IMG)

.PHONY: bundle-run
bundle-run: # Install bundle on cluster using operator sdk.
	oc create ns $(KPM_NAMESPACE)
	$(OPERATOR_SDK) --security-context-config restricted -n $(KPM_NAMESPACE) run bundle $(BUNDLE_IMG)

.PHONY: bundle-clean
bundle-clean: # Uninstall bundle on cluster using operator sdk.
	$(OPERATOR_SDK) cleanup $(PROJECT_NAME) -n $(KPM_NAMESPACE)
	oc delete ns $(KPM_NAMESPACE)

.PHONY: operator-sdk
operator-sdk: $(OPERATOR_SDK) ## Download operator-sdk locally if necessary.
$(OPERATOR_SDK): $(LOCALBIN)
	@if test -x $(OPERATOR_SDK) && ! $(OPERATOR_SDK) version 2>/dev/null | grep -q "$(OPERATOR_SDK_VERSION)$$"; then \
		echo "$(OPERATOR_SDK) version is not expected $(OPERATOR_SDK_VERSION). Removing it before installing."; \
		rm -rf $(OPERATOR_SDK); \
	fi
	@if [ ! -f $(OPERATOR_SDK) ]; then \
		set -e ;\
		echo "Downloading operator-sdk $(OPERATOR_SDK_VERSION)..." ;\
		OS=$$(go env GOOS) && ARCH=$$(go env GOARCH) && \
		curl -sSLo $(OPERATOR_SDK) https://github.com/operator-framework/operator-sdk/releases/download/v$(OPERATOR_SDK_VERSION)/operator-sdk_$${OS}_$${ARCH} ;\
		chmod +x $(OPERATOR_SDK) ;\
		echo "operator-sdk downloaded successfully." ;\
	fi

.PHONY: opm
opm: $(OPM) ## Download opm locally if necessary.
$(OPM): $(LOCALBIN)
	@if test -x $(OPM) && ! $(OPM) version 2>/dev/null | grep -q "$(OPM_VERSION)$$"; then \
		echo "$(OPM) version is not expected $(OPM_VERSION). Removing it before installing."; \
		rm -rf $(OPM); \
	fi
	@if [ ! -f $(OPM) ]; then \
		set -e ;\
		echo "Downloading opm $(OPM_VERSION)..." ;\
		OS=$$(go env GOOS) && ARCH=$$(go env GOARCH) && \
		curl -sSLo $(OPM) https://github.com/operator-framework/operator-registry/releases/download/$(OPM_VERSION)/$${OS}-$${ARCH}-opm ;\
		chmod +x $(OPM) ;\
		echo "opm downloaded successfully." ;\
	fi

.PHONY: catalog-build
catalog-build: opm ## Build a catalog image.
	$(OPM) index add --container-tool $(IMGTOOL) --mode semver --tag $(CATALOG_IMG) --bundles $(BUNDLE_IMGS) $(if ifeq $(TLS_VERIFY) false, --skip-tls) $(FROM_INDEX_OPT)

# Push the catalog image.
.PHONY: catalog-push
catalog-push: ## Push a catalog image.
	 $(IMGTOOL) push ${CATALOG_IMG}

coverage:
	go test -v -coverprofile=coverage.out ./controllers/ ./pkg/...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Average code coverage: $$(go tool cover -func coverage.out | awk 'END {print $$3}' | sed 's/\..*//')%" 
	@if [ $$(go tool cover -func coverage.out | awk 'END {print $$3}' | sed 's/\..*//') -lt 85 ]; then \
                echo "Total unit test coverage below 85%"; false; \
        fi

tidy:
	go mod tidy

verify-test: tidy
	go test -count=1 -v ./...

race: tidy
	CGO_ENABLED=1 go test -count=1 -race -v ./...

clean:
	go clean --cache
	rm -r build/bin/manager
	rm -r build/bin/nodeagent

gofmt:
	gofmt -w .

update:
	sed -i 's|image: .*|image: $(IMG)|' config/manager/manager.yaml
	sed -i 's|image: .*|image: $(IMG)|' config/manager/ocp/manager.yaml
	sed -i 's|image: .*|image: $(IMG_AGENT)|' build/manifests/power-node-agent-ds.yaml
	sed -i 's|image: .*|image: $(IMG_AGENT)|' build/manifests/ocp/power-node-agent-ds.yaml

# markdownlint rules, following: https://github.com/openshift/enhancements/blob/master/Makefile
.PHONY: markdownlint-image
markdownlint-image:  ## Build local container markdownlint-image
	$(IMGTOOL) image build -f ./hack/Dockerfile.markdownlint --tag $(IMAGE_NAME)-markdownlint:latest ./hack

.PHONY: markdownlint-image-clean
markdownlint-image-clean:  ## Remove locally cached markdownlint-image
	$(IMGTOOL) image rm $(IMAGE_NAME)-markdownlint:latest

markdownlint: markdownlint-image  ## run the markdown linter
	$(IMGTOOL) run \
		--rm=true \
		--env RUN_LOCAL=true \
		--env VALIDATE_MARKDOWN=true \
		--env PULL_BASE_SHA=$(PULL_BASE_SHA) \
		-v $$(pwd):/workdir:Z \
		$(IMAGE_NAME)-markdownlint:latest
