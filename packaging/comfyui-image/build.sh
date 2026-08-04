#!/usr/bin/env bash
set -euo pipefail

# The Spark that builds the image. There is no default: hardcoding one
# operator's machine into a public repository leaks their network and sends
# everyone else's build to a host they do not own.
readonly REMOTE_HOST="${COMFYUI_BUILD_HOST:?set COMFYUI_BUILD_HOST to the ssh target of the Spark that should build the image, for example user@spark-head}"
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
sudo docker build --build-arg MAX_JOBS=4 --tag "${image_tag}" .
sudo docker image inspect "${image_tag}" --format 'Image ID: {{.Id}}'
sudo docker image inspect "${image_tag}" --format 'Size: {{.Size}} bytes'
REMOTE
