#!/usr/bin/env bash
# image/build.sh [version]
#
# Build the sandboxd base image: a Debian-slim image with the language
# toolchains baked in. There is no in-sandbox supervisor.
#
# Build context is the REPO ROOT (one level up from this script). Native arch
# (works on both arm64 and amd64). Override the tag by passing a version.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VERSION="${1:-0.3.0}"
IMAGE="${SANDBOXD_IMAGE:-sandboxd-base:${VERSION}}"

# `docker` may need sudo on some hosts; honour a DOCKER override that can
# include arguments (e.g. DOCKER="sudo docker").
DOCKER="${DOCKER:-docker}"

echo "Building ${IMAGE} (context: ${REPO_ROOT}) ..."
$DOCKER build \
  -f "${REPO_ROOT}/image/Dockerfile" \
  -t "${IMAGE}" \
  "${REPO_ROOT}"

SIZE_MB=$($DOCKER image inspect "${IMAGE}" \
  --format '{{.Size}}' \
  | awk '{ printf "%.1f", $1/1024/1024 }')

echo "Built ${IMAGE} (${SIZE_MB} MB uncompressed)"
