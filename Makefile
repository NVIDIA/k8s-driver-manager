# Copyright (c) 2021, NVIDIA CORPORATION.  All rights reserved.
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


.PHONY: all build builder test
.DEFAULT_GOAL := all

##### Global variables #####

CUDA_VERSION ?= 11.4.1
DOCKER ?= docker
ifeq ($(IMAGE),)
REGISTRY ?= nvidia
IMAGE := $(REGISTRY)/k8s-driver-manager
endif

# Must be set externally before invoking
VERSION ?= v0.1.0

##### Public rules #####
TARGETS := ubi8

DEFAULT_PUSH_TARGET := ubi8

PUSH_TARGETS := $(patsubst %, push-%, $(TARGETS))
BUILD_TARGETS := $(patsubst %, build-%, $(TARGETS))

.PHONY: $(TARGETS) $(PUSH_TARGETS) $(BUILD_TARGETS)

all: $(TARGETS)

push-all: $(PUSH_TARGETS)
build-all: $(BUILD_TARGETS)

$(PUSH_TARGETS): push-%:
	$(DOCKER) push "$(IMAGE):$(VERSION)-$(*)"

# For the default push target we also push a short tag equal to the version.
# We skip this for the development release
RELEASE_DEVEL_TAG ?= devel
ifneq ($(strip $(VERSION)),$(RELEASE_DEVEL_TAG))
push-$(DEFAULT_PUSH_TARGET): push-short
endif
push-short:
	$(DOCKER) tag "$(IMAGE):$(VERSION)-$(DEFAULT_PUSH_TARGET)" "$(IMAGE):$(VERSION)"
	$(DOCKER) push "$(IMAGE):$(VERSION)"

build-ubi8: DOCKERFILE_SUFFIX := ubi8
build-ubi8: BASE_DIST := ubi8

# Both ubi8 and build-ubi8 trigger a build of the relevant image
$(TARGETS): %: build-%
$(BUILD_TARGETS): build-%:
	$(DOCKER) build --pull \
		--tag $(IMAGE):$(VERSION)-$(*) \
		--build-arg BASE_DIST="$(BASE_DIST)" \
		--build-arg CUDA_VERSION="$(CUDA_VERSION)" \
		--build-arg VERSION="$(VERSION)" \
		--file docker/Dockerfile.$(DOCKERFILE_SUFFIX) .

.PHONY: bump-commit
BUMP_COMMIT := Bump to version $(VERSION)
bump-commit:
	@git log | if [ ! -z "$$(grep -o '$(BUMP_COMMIT)' | sort -u)" ]; then \
		echo "\nERROR: '$(BUMP_COMMIT)' already committed\n"; \
		exit 1; \
	fi
	@git add Makefile
	@git commit -m "$(BUMP_COMMIT)"
	@echo "Applied the diff:"
	@git --no-pager diff HEAD~1
