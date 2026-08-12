# Copyright (c) 2020-2022, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

DOCKER   ?= docker
MKDIR    ?= mkdir
GO       ?= go

MODULE := github.com/NVIDIA/k8s-driver-manager

include $(CURDIR)/versions.mk

ifeq ($(IMAGE_NAME),)
REGISTRY ?= nvidia
IMAGE_NAME = $(REGISTRY)/k8s-driver-manager
endif

CHECK_TARGETS := lint
MAKE_TARGETS := build check fmt lint-internal test check-vendor third-party-notices check-third-party-notices $(CHECK_TARGETS)

TARGETS := $(MAKE_TARGETS)

DOCKER_TARGETS := $(patsubst %,docker-%, $(TARGETS))
.PHONY: $(TARGETS) $(DOCKER_TARGETS)

GOOS ?= linux

build:
	GOOS=$(GOOS) go build ./...

all: check test build
check: $(CHECK_TARGETS)

# Apply go fmt to the codebase
fmt:
	go list -f '{{.Dir}}' $(MODULE)/... \
		| xargs gofmt -s -l -w

goimports:
	go list -f {{.Dir}} $(MODULE)/... \
		| xargs goimports -local $(MODULE) -w

lint:
	golangci-lint run ./...

BIN_DIR := $(CURDIR)/bin
GO_LICENSES := $(BIN_DIR)/go-licenses

$(GO_LICENSES): deployments/devel/go.mod deployments/devel/go.sum
	cd $(CURDIR)/deployments/devel \
		&& GOFLAGS=-mod=readonly GOBIN=$(BIN_DIR) go install github.com/google/go-licenses/v2

third-party-notices: $(GO_LICENSES)
	@bash scripts/generate-third-party-notices.sh

check-third-party-notices: third-party-notices
	@echo "- Checking if THIRD_PARTY_NOTICES.md is up to date..."
	@git ls-files --error-unmatch THIRD_PARTY_NOTICES.md >/dev/null 2>&1 \
		|| { echo "ERROR: THIRD_PARTY_NOTICES.md is not tracked. Run 'make third-party-notices' and commit the result."; exit 1; }
	@git diff --exit-code -- THIRD_PARTY_NOTICES.md \
		|| { echo "ERROR: THIRD_PARTY_NOTICES.md is stale. Run 'make third-party-notices' and commit the change."; exit 1; }

COVERAGE_FILE := coverage.out
test: build
	go test -coverprofile=$(COVERAGE_FILE).with-mocks $(MODULE)/...

coverage: test
	cat $(COVERAGE_FILE).with-mocks | grep -v "_mock.go" > $(COVERAGE_FILE)
	go tool cover -func=$(COVERAGE_FILE)

$(DOCKER_TARGETS): docker-%:
	@echo "Running 'make $(*)' in container image $(BUILDIMAGE)"
	$(DOCKER) run \
		--rm \
		-e GOCACHE=/tmp/.cache/go \
		-e GOMODCACHE=/tmp/.cache/gomod \
		-v $(PWD):/work \
		-w /work \
		--user $$(id -u):$$(id -g) \
		$(BUILDIMAGE) \
			make $(*)

# Start an interactive shell using the development image.
PHONY: .shell
.shell:
	$(DOCKER) run \
		--rm \
		-ti \
		-e GOCACHE=/tmp/.cache/go \
		-e GOMODCACHE=/tmp/.cache/gomod \
		-v $(PWD):/work \
		-w /work \
		--user $$(id -u):$$(id -g) \
		$(BUILDIMAGE)

vendor:  | mod-tidy mod-vendor mod-verify

mod-tidy:
	@for mod in $$(find . -name go.mod -not -path "./testdata/*" -not -path "./third_party/*"); do \
	    echo "Tidying $$mod..."; ( \
	        cd $$(dirname $$mod) && go mod tidy \
            ) || exit 1; \
	done

mod-vendor:
	@for mod in $$(find . -name go.mod -not -path "./testdata/*" -not -path "./third_party/*" -not -path "./deployments/*"); do \
		echo "Vendoring $$mod..."; ( \
			cd $$(dirname $$mod) && go mod vendor \
			) || exit 1; \
	done

mod-verify:
	@for mod in $$(find . -name go.mod -not -path "./testdata/*" -not -path "./third_party/*"); do \
	    echo "Verifying $$mod..."; ( \
	        cd $$(dirname $$mod) && go mod verify | sed 's/^/  /g' \
	    ) || exit 1; \
	done

check-vendor: vendor
	git diff --exit-code HEAD -- go.mod go.sum vendor
