export APP_NAME=intel-kubernetes-power-manager
# Current Operator version
VERSION ?= 2.5.0
# parameter used for helm chart image
HELM_CHART ?= v2.5.0
HELM_VERSION := $(shell echo $(HELM_CHART) | cut -d "v" -f2)
# used to detemine if certain targets should build for openshift
OCP ?= false

IMAGE_NAME ?= power-operator
IMAGE_TAG_BASE ?= docker.io/intel/$(IMAGE_NAME)

# Image URL to use all building/pushing image targets
IMG ?= $(IMAGE_TAG_BASE)-operator:$(VERSION)
IMG_AGENT ?= docker.io/intel/power-node-agent:$(VERSION)
# Default bundle image tag
BUNDLE_IMG ?= intel-kubernetes-power-manager-bundle:$(VERSION)
# version of ocp being supported
OCP_VERSION=4.18
# image used for building the dockerfile for ocp
OCP_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal:9.5-1742914212
# Platform to build the images for.
PLATFORM ?= linux/amd64
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

## Tool Versions
KUSTOMIZE_VERSION ?= v5.4.3
CONTROLLER_TOOLS_VERSION ?= v0.17.2

.PHONY: kubectl
kubectl: $(KUBECTL) ## Use envtest to download kubectl
$(KUBECTL): $(LOCALBIN) envtest
	if [ ! -f $(KUBECTL) ]; then \
		KUBEBUILDER_ASSETS=$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path); \
		ln -sf $${KUBEBUILDER_ASSETS}/kubectl $(KUBECTL); \
	fi

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary. If wrong version is installed, it will be removed before downloading.
$(KUSTOMIZE): $(LOCALBIN)
	@if test -x $(LOCALBIN)/kustomize && ! $(LOCALBIN)/kustomize version | grep -q $(KUSTOMIZE_VERSION); then \
		echo "$(LOCALBIN)/kustomize version is not expected $(KUSTOMIZE_VERSION). Removing it before installing."; \
		rm -rf $(LOCALBIN)/kustomize; \
	fi
	test -s $(LOCALBIN)/kustomize || GOBIN=$(LOCALBIN) GO111MODULE=on go install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary. If wrong version is installed, it will be overwritten.
$(CONTROLLER_GEN): $(LOCALBIN)
	test -s $(LOCALBIN)/controller-gen && $(LOCALBIN)/controller-gen --version | grep -q $(CONTROLLER_TOOLS_VERSION) || \
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

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

ifneq (, $(IMAGE_REGISTRY))
IMAGE_TAG_BASE = $(IMAGE_REGISTRY)/$(APP_NAME)
else
IMAGE_TAG_BASE = $(APP_NAME)
endif

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
	 $(IMGTOOL) build -f build/Dockerfile --platform $(PLATFORM) -t intel/power-operator:v$(VERSION) .
	 $(IMGTOOL) build -f build/Dockerfile.nodeagent --platform $(PLATFORM) -t intel/power-node-agent:v$(VERSION) .

images-ocp: generate manifests
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
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

# Build the Manager's image
build-controller:
	$(IMGTOOL) build -f build/Dockerfile --platform $(PLATFORM) -t intel-power-operator .

# Build the Node Agent's image
build-agent:
	$(IMGTOOL) build -f build/Dockerfile.nodeagent --platform $(PLATFORM) -t intel-power-node-agent .

build-controller-ocp:
	$(IMGTOOL) build --build-arg="BASE_IMAGE=$(OCP_IMAGE)" -f build/Dockerfile --platform $(PLATFORM) -t intel/power-operator_ocp-$(OCP_VERSION):v$(VERSION) .

build-agent-ocp:
	$(IMGTOOL) build --build-arg="BASE_IMAGE=$(OCP_IMAGE)" -f build/Dockerfile.nodeagent --platform $(PLATFORM) -t intel/power-node-agent_ocp-$(OCP_VERSION):v$(VERSION) .

.PHONY: docker-push controller-gen kustomize bundle bundle-build bundle-push
# Push the image
push:
	$(IMGTOOL) push ${IMG}

# Generate bundle manifests and metadata, then validate generated files.
bundle: update manifests kustomize
# directory used to get image name for bundle
ifeq (false, $(OCP))
	sed -i 's/\.\.\/manager.*$$/\.\.\/manager/' config/default/kustomization.yaml
else
	sed -i 's/\.\.\/manager.*$$/\.\.\/manager\/ocp/' config/default/kustomization.yaml
	sed -i 's/- .*rbac\.yaml/- \.\/ocp\/rbac.yaml/' config/rbac/kustomization.yaml
	sed -i 's/- .*role\.yaml/- \.\/ocp\/role.yaml/' config/rbac/kustomization.yaml
endif
	operator-sdk generate kustomize manifests -q
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/manifests | operator-sdk generate bundle -q --use-image-digests --overwrite --version $(VERSION) $(BUNDLE_METADATA_OPTS) 
	operator-sdk bundle validate ./bundle

# Build the bundle image.
bundle-build:
	$(IMGTOOL) build -f bundle.Dockerfile -t $(BUNDLE_IMG) .
# Push bundle image
bundle-push:
	$(IMGTOOL) push $(BUNDLE_IMG)
.PHONY: opm catalog-build catalog-push coverage update
OPM_VERSION = v1.26.2
OPM = ./bin/opm
opm: ## Download opm locally if necessary.
ifeq (,$(wildcard $(OPM)))
ifeq (,$(shell which opm 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPM)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPM) https://github.com/operator-framework/operator-registry/releases/download/${OPM_VERSION}/$${OS}-$${ARCH}-opm ;\
	chmod +x $(OPM) ;\
	}
else
OPM = $(shell which opm)
endif
endif

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
