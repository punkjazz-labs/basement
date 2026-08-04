#!/usr/bin/env bash
set -euo pipefail

readonly REMOTE_HOST="spark@spark-host.invalid"
readonly REMOTE_DIR="/tmp/comfyui-image-build"
readonly IMAGE_TAG="basement-comfyui:v0.30.0"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR

rsync --archive --compress --delete "${SCRIPT_DIR}/" "${REMOTE_HOST}:${REMOTE_DIR}/"

ssh "${REMOTE_HOST}" bash -s -- "${REMOTE_DIR}" "${IMAGE_TAG}" <<'REMOTE'
set -euo pipefail

readonly remote_dir="$1"
readonly image_tag="$2"

cd "${remote_dir}"
docker build --build-arg MAX_JOBS=4 --tag "${image_tag}" .
docker image inspect "${image_tag}" --format 'Image ID: {{.Id}}'
docker image inspect "${image_tag}" --format 'Size: {{.Size}} bytes'
REMOTE
