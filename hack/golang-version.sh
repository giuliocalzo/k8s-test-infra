#!/bin/bash
# Copyright 2025 NVIDIA CORPORATION
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

set -euo pipefail

REPO_ROOT="$( cd "$( dirname "${BASH_SOURCE[0]}" )"/.. && pwd )"
GO_MOD="${REPO_ROOT}/go.mod"

# The `go` directive in go.mod is the single source of truth for the Go version
# used across builds and CI. It is pinned to a full x.y.z so the output doubles
# as a golang container image tag.
GOLANG_VERSION=$(awk '$1 == "go" { print $2; exit }' "${GO_MOD}")

if [[ ! "${GOLANG_VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "golang-version.sh: could not read an x.y.z go directive from ${GO_MOD} (got '${GOLANG_VERSION}')" >&2
    exit 1
fi

echo "${GOLANG_VERSION}"
