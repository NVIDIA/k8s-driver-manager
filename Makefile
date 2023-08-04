# Copyright (c) 2023, NVIDIA CORPORATION.  All rights reserved.
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

GO_CMD ?= go

validate-modules:
	@echo "- Verifying that the dependencies have expected content..."
	$(GO_CMD) mod verify
	@echo "- Checking for any unused/missing packages in go.mod..."
	$(GO_CMD) mod tidy
	@git diff --exit-code -- go.sum go.mod
	@echo "- Checking if the vendor dir is in sync..."
	$(GO_CMD) mod vendor
	@git diff --exit-code -- vendor
