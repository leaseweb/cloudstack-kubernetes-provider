# Licensed to the Apache Software Foundation (ASF) under one
# or more contributor license agreements.  See the NOTICE file
# distributed with this work for additional information
# regarding copyright ownership.  The ASF licenses this file
# to you under the Apache License, Version 2.0 (the
# "License"); you may not use this file except in compliance
# with the License.  You may obtain a copy of the License at
#
#   http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.

GOOS	?= $(shell go env GOOS)
GOPROXY	?= $(shell go env GOPROXY)
GOARCH	:=

BUILD_DATE=$(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
GIT_COMMIT=$(shell git rev-parse HEAD)
GIT_COMMIT_SHORT=$(shell git rev-parse --short HEAD)
GIT_TAG=$(shell git describe --abbrev=0 --tags 2>/dev/null || echo v0.0.0)
GIT_IS_TAG=$(shell git describe --exact-match --abbrev=0 --tags 2>/dev/null || echo NOT_A_TAG)
GIT_VERSION?=$(shell git describe --dirty --tags --match='v*')
LDFLAGS="-X 'k8s.io/component-base/version.gitVersion=${GIT_VERSION}' -X 'k8s.io/component-base/version.gitCommit=${GIT_COMMIT}' -X 'k8s.io/component-base/version.buildDate=${BUILD_DATE}'"

CMD_SRC=\
	cmd/cloudstack-ccm/main.go

export REPO_ROOT := $(shell git rev-parse --show-toplevel)

# Directories
TOOLS_DIR := $(REPO_ROOT)/hack/tools
TOOLS_BIN_DIR := $(TOOLS_DIR)/bin
BIN_DIR ?= bin

GO_INSTALL := ./hack/go_install.sh

GOLANGCI_LINT_BIN := golangci-lint
GOLANGCI_LINT_VER := v1.64.8
GOLANGCI_LINT := $(abspath $(TOOLS_BIN_DIR)/$(GOLANGCI_LINT_BIN)-$(GOLANGCI_LINT_VER))
GOLANGCI_LINT_PKG := github.com/golangci/golangci-lint/cmd/golangci-lint

##@ Linting
## --------------------------------------
## Linting
## --------------------------------------

.PHONY: fmt
fmt: ## Run go fmt on the whole project.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet on the whole project.
	go vet ./...

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run linting for the project.
	$(MAKE) fmt
	$(MAKE) vet
	$(GOLANGCI_LINT) run -v --timeout 360s ./...

##@ Build
## --------------------------------------
## Build
## --------------------------------------

.PHONY: all clean docker

all: cloudstack-ccm

clean:
	rm -f cloudstack-ccm

cloudstack-ccm: ${CMD_SRC}
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) GOPROXY=${GOPROXY} go build \
		-trimpath \
		-ldflags ${LDFLAGS} \
	 	-o $@ $^

test:
	go test -v ./...
	go vet ./...
	@(echo "gofmt -l"; FMTFILES="$$(gofmt -l .)"; if test -n "$${FMTFILES}"; then echo "Go files that need to be reformatted (use 'go fmt'):\n$${FMTFILES}"; exit 1; fi)

docker:
	docker build . -t ghcr.io/leaseweb/cloudstack-kubernetes-provider:${GIT_COMMIT_SHORT}
	docker tag ghcr.io/leaseweb/cloudstack-kubernetes-provider:${GIT_COMMIT_SHORT} ghcr.io/leaseweb/cloudstack-kubernetes-provider:latest
ifneq (${GIT_IS_TAG},NOT_A_TAG)
	docker tag ghcr.io/leaseweb/cloudstack-kubernetes-provider:${GIT_COMMIT_SHORT} ghcr.io/leaseweb/cloudstack-kubernetes-provider:${GIT_TAG}
endif

##@ hack/tools:

.PHONY: $(GOLANGCI_LINT_BIN)
$(GOLANGCI_LINT_BIN): $(GOLANGCI_LINT) ## Build a local copy of golangci-lint.

$(GOLANGCI_LINT): # Build golangci-lint from tools folder.
	GOBIN=$(TOOLS_BIN_DIR) $(GO_INSTALL) $(GOLANGCI_LINT_PKG) $(GOLANGCI_LINT_BIN) $(GOLANGCI_LINT_VER)
