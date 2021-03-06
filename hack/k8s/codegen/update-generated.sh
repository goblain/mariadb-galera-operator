#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

DOCKER_REPO_ROOT="/go/src/github.com/dansksupermarked/mariadb-galera-operator"
IMAGE=${IMAGE:-"mariadb-operator/codegen"}

docker run --rm \
  -v "$PWD":"$DOCKER_REPO_ROOT" \
  -w "$DOCKER_REPO_ROOT" \
  "$IMAGE" \
  "./hack/k8s/codegen/codegen.sh" \
  "all" \
  "github.com/dansksupermarked/mariadb-galera-operator/pkg/generated" \
  "github.com/dansksupermarked/mariadb-galera-operator/pkg/apis" \
  "components:v1alpha1" \
  --go-header-file "./hack/k8s/codegen/boilerplate.go.txt" \
$@