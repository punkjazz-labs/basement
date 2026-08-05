#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf '%s\n' \
    'Run the MiniMax H3 native and two-pass hardware comparison.' \
    '' \
    'Required environment:' \
    '  H3_EVAL_HOST          SSH target, for example user@spark-head' \
    '  H3_EVAL_MODEL_DIR     Model directory on the target host' \
    '  H3_EVAL_RESULTS_DIR   Local directory for videos and receipts' \
    '  H3_EVAL_PROMPT        One prompt used by every run' \
    '  H3_EVAL_SEED          One integer seed used by every run' \
    '  H3_EVAL_SPLIT_STEP    First-pass schedule split, from 1 through 19' \
    '' \
    'Optional environment:' \
    '  H3_EVAL_AUDIO_DENOISE Audio re-noise strength, default 1.0' \
    '  H3_EVAL_IMAGE         Pinned ComfyUI image reference'
}

if [[ "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

readonly REMOTE_HOST="${H3_EVAL_HOST:?set H3_EVAL_HOST to the SSH target, for example user@spark-head}"
readonly MODEL_DIR="${H3_EVAL_MODEL_DIR:?set H3_EVAL_MODEL_DIR to the model directory on the target host}"
readonly RESULTS_DIR="${H3_EVAL_RESULTS_DIR:?set H3_EVAL_RESULTS_DIR to a local results directory}"
readonly EVAL_PROMPT="${H3_EVAL_PROMPT:?set H3_EVAL_PROMPT to the prompt shared by every run}"
readonly EVAL_SEED="${H3_EVAL_SEED:?set H3_EVAL_SEED to the integer seed shared by every run}"
readonly SPLIT_STEP="${H3_EVAL_SPLIT_STEP:?set H3_EVAL_SPLIT_STEP to a value from 1 through 19}"
readonly AUDIO_DENOISE="${H3_EVAL_AUDIO_DENOISE:-1.0}"
readonly EVAL_IMAGE="${H3_EVAL_IMAGE:-ghcr.io/punkjazz-labs/basement-comfyui@sha256:8e6715f3e133c03b12f7730c4d66124554952bf9dae81263a153be05f96d23a9}"
readonly CANDIDATE_COMMIT="2b4f7d6e2edf5ac3c1c553efac9d373aeafa59bd"

if [[ "$REMOTE_HOST" == -* ]]; then
  printf 'H3_EVAL_HOST must not start with a dash\n' >&2
  exit 2
fi
if [[ ! "$EVAL_SEED" =~ ^[0-9]+$ ]]; then
  printf 'H3_EVAL_SEED must be a non-negative integer\n' >&2
  exit 2
fi
if [[ ! "$SPLIT_STEP" =~ ^[0-9]+$ ]] || ((SPLIT_STEP < 1 || SPLIT_STEP > 19)); then
  printf 'H3_EVAL_SPLIT_STEP must be an integer from 1 through 19\n' >&2
  exit 2
fi
if [[ ! "$AUDIO_DENOISE" =~ ^[0-9]+([.][0-9]+)?$ ]] \
  || ! awk -v value="$AUDIO_DENOISE" 'BEGIN { exit !(value >= 0 && value <= 1) }'; then
  printf 'H3_EVAL_AUDIO_DENOISE must be from 0 through 1\n' >&2
  exit 2
fi

for local_command in curl jq rsync ssh; do
  if ! command -v "$local_command" >/dev/null 2>&1; then
    printf 'Required local command is missing: %s\n' "$local_command" >&2
    exit 2
  fi
done

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
readonly REPO_ROOT
readonly NATIVE_GRAPH="${REPO_ROOT}/internal/recipe/graphs/minimax-h3-t2v.json"

if [[ ! -f "$NATIVE_GRAPH" ]]; then
  printf 'Native H3 graph is missing: %s\n' "$NATIVE_GRAPH" >&2
  exit 2
fi

LOCAL_STAGE="$(mktemp -d)"
readonly LOCAL_STAGE
# shellcheck disable=SC2329 # Invoked by the EXIT trap.
cleanup_local() {
  rm -rf -- "$LOCAL_STAGE"
}
trap cleanup_local EXIT

mkdir -p "$LOCAL_STAGE/candidate" "$LOCAL_STAGE/graphs" "$RESULTS_DIR"

readonly CANDIDATE_BASE="https://raw.githubusercontent.com/Tr1dae/ComfyUI-MiniMaxH3_LatentUpscaler/${CANDIDATE_COMMIT}"
for candidate_file in __init__.py nodes.py utils.py; do
  curl --fail --silent --show-error --location \
    "${CANDIDATE_BASE}/${candidate_file}" \
    --output "$LOCAL_STAGE/candidate/$candidate_file"
done

make_native_graph() {
  local label="$1"
  local width="$2"
  local height="$3"
  local output="$4"

  jq \
    --arg prompt "$EVAL_PROMPT" \
    --arg prefix "evaluation/${label}" \
    --argjson seed "$EVAL_SEED" \
    --argjson width "$width" \
    --argjson height "$height" \
    '
      .["104"].inputs.prompt = $prompt
      | .["104"].inputs.width = $width
      | .["104"].inputs.height = $height
      | .["104"].inputs.length = 124
      | .["15"].inputs.noise_seed = $seed
      | .["92"].inputs.filename_prefix = $prefix
    ' "$NATIVE_GRAPH" >"$output"
}

make_two_pass_graph() {
  local label="$1"
  local low_width="$2"
  local low_height="$3"
  local scale_numerator="$4"
  local scale_denominator="$5"
  local output="$6"

  jq \
    --arg prompt "$EVAL_PROMPT" \
    --arg prefix "evaluation/${label}" \
    --argjson seed "$EVAL_SEED" \
    --argjson low_width "$low_width" \
    --argjson low_height "$low_height" \
    --argjson scale_numerator "$scale_numerator" \
    --argjson scale_denominator "$scale_denominator" \
    --argjson split_step "$SPLIT_STEP" \
    --argjson audio_denoise "$AUDIO_DENOISE" \
    '
      .["104"].inputs.prompt = $prompt
      | .["104"].inputs.width = $low_width
      | .["104"].inputs.height = $low_height
      | .["104"].inputs.length = 124
      | .["15"].inputs.noise_seed = $seed
      | .["92"].inputs.filename_prefix = $prefix
      | .["105"] = {
          "class_type": "SplitSigmas",
          "inputs": {"sigmas": ["9", 0], "step": $split_step}
        }
      | .["14"].inputs.sigmas = ["105", 0]
      | .["106"] = {
          "class_type": "MiniMaxH3LatentUpscaleCombined",
          "inputs": {
            "samples": ["14", 1],
            "scale_by": ($scale_numerator / $scale_denominator),
            "method": "bilinear",
            "model": ["6", 0],
            "noise": ["15", 0],
            "sigmas": ["105", 1],
            "audio_denoise": $audio_denoise,
            "positive": ["104", 0]
          }
        }
      | .["107"] = {
          "class_type": "BasicGuider",
          "inputs": {"model": ["6", 0], "conditioning": ["106", 1]}
        }
      | .["108"] = {"class_type": "DisableNoise", "inputs": {}}
      | .["109"] = {
          "class_type": "SamplerCustomAdvanced",
          "inputs": {
            "noise": ["108", 0],
            "guider": ["107", 0],
            "sampler": ["17", 0],
            "sigmas": ["105", 1],
            "latent_image": ["106", 0]
          }
        }
      | .["10"].inputs.samples = ["109", 0]
      | .["23"].inputs.samples = ["109", 0]
    ' "$NATIVE_GRAPH" >"$output"
}

make_native_graph \
  native-1920x1088 1920 1088 \
  "$LOCAL_STAGE/graphs/native-1920x1088.json"
make_two_pass_graph \
  two-pass-1920x1088 960 544 2 1 \
  "$LOCAL_STAGE/graphs/two-pass-1920x1088.json"
make_native_graph \
  native-2560x1440 2560 1440 \
  "$LOCAL_STAGE/graphs/native-2560x1440.json"
# The QHD source grid is close to half size. 160 / 82 maps its latent width
# exactly to the target. The node's even-grid snap maps latent height 46 to 90.
make_two_pass_graph \
  two-pass-2560x1440 1312 736 160 82 \
  "$LOCAL_STAGE/graphs/two-pass-2560x1440.json"

jq -n \
  --arg candidate_commit "$CANDIDATE_COMMIT" \
  --arg image "$EVAL_IMAGE" \
  --argjson seed "$EVAL_SEED" \
  --argjson split_step "$SPLIT_STEP" \
  --argjson audio_denoise "$AUDIO_DENOISE" \
  '{
    candidate_commit: $candidate_commit,
    image: $image,
    frames: 124,
    fps: 24,
    seed: $seed,
    split_step: $split_step,
    audio_denoise: $audio_denoise,
    interpolation: "bilinear",
    cases: [
      {label: "native-1920x1088", output: [1920, 1088]},
      {label: "two-pass-1920x1088", first_pass: [960, 544], output: [1920, 1088]},
      {label: "native-2560x1440", output: [2560, 1440]},
      {label: "two-pass-2560x1440", first_pass: [1312, 736], output: [2560, 1440]}
    ]
  }' >"$LOCAL_STAGE/manifest.json"

REMOTE_ROOT="$(ssh "$REMOTE_HOST" 'mktemp -d /tmp/basement-h3-two-pass.XXXXXX')"
REMOTE_ROOT="${REMOTE_ROOT//$'\r'/}"
REMOTE_ROOT="${REMOTE_ROOT//$'\n'/}"
readonly REMOTE_ROOT
if [[ ! "$REMOTE_ROOT" =~ ^/tmp/basement-h3-two-pass\.[A-Za-z0-9]+$ ]]; then
  printf 'Target returned an unexpected work directory: %s\n' "$REMOTE_ROOT" >&2
  exit 2
fi

rsync --archive "$LOCAL_STAGE/" "${REMOTE_HOST}:${REMOTE_ROOT}/"

set +e
ssh "$REMOTE_HOST" bash -s -- \
  "$REMOTE_ROOT" "$EVAL_IMAGE" "$MODEL_DIR" "$AUDIO_DENOISE" <<'REMOTE' \
  | tee "$RESULTS_DIR/results.txt"
set -euo pipefail

readonly work_root="$1"
readonly image_ref="$2"
readonly model_dir="$3"
readonly audio_denoise="$4"
readonly candidate_dir="${work_root}/candidate"
readonly graph_dir="${work_root}/graphs"
readonly output_dir="${work_root}/outputs"

for remote_command in awk curl jq; do
  if ! command -v "$remote_command" >/dev/null 2>&1; then
    printf 'Required target command is missing: %s\n' "$remote_command" >&2
    exit 2
  fi
done
if [[ ! -d "$model_dir" ]]; then
  printf 'Model directory does not exist on the target: %s\n' "$model_dir" >&2
  exit 2
fi

if docker info >/dev/null 2>&1; then
  docker_command=(docker)
elif sudo -n docker info >/dev/null 2>&1; then
  docker_command=(sudo -n docker)
else
  printf 'Docker is not available to the target user or passwordless sudo\n' >&2
  exit 2
fi
readonly -a docker_command

if ! "${docker_command[@]}" image inspect "$image_ref" >/dev/null 2>&1; then
  printf 'Pinned evaluation image is not present on the target: %s\n' "$image_ref" >&2
  exit 2
fi

mkdir -p "$output_dir"
current_container=''
cleanup_remote() {
  if [[ -n "$current_container" ]]; then
    "${docker_command[@]}" rm --force "$current_container" >/dev/null 2>&1 || true
  fi
  rm -rf -- "$candidate_dir"
}
trap cleanup_remote EXIT

meminfo_value() {
  local key="$1"
  awk -v key="${key}:" '$1 == key { print $2; exit }' /proc/meminfo
}

container_is_running() {
  local container_name="$1"
  [[ "$("${docker_command[@]}" inspect --format '{{.State.Running}}' "$container_name" 2>/dev/null)" == "true" ]]
}

run_case() {
  local label="$1"
  local graph_file="$2"
  local run_dir="${work_root}/runs/${label}"
  local memory_log="${run_dir}/memory-available-kb.log"
  local request_file="${run_dir}/request.json"
  local history_file="${run_dir}/history.json"
  local container_name="h3-eval-${label//[^a-zA-Z0-9]/-}-$$"
  local total_kb baseline_available_kb idle_available_kb peak_used_kb
  local start_seconds end_seconds wall_seconds monitor_pid host_port
  local prompt_response prompt_id history_response completed status_string
  local relative_video container_video video_probe video_width video_height video_frames video_status
  local expected_width expected_height audio_codec audio_duration audio_status
  local garble_check case_status case_error

  mkdir -p \
    "$run_dir/input" \
    "$run_dir/temp" \
    "$run_dir/user" \
    "$output_dir"

  total_kb="$(meminfo_value MemTotal)"
  baseline_available_kb="$(meminfo_value MemAvailable)"
  idle_available_kb='n/a'
  audio_codec='n/a'
  audio_duration='n/a'
  audio_status='failed'
  video_width='n/a'
  video_height='n/a'
  video_frames='n/a'
  video_status='failed'
  garble_check='listen_required'
  case_status='success'
  case_error=''
  start_seconds="$(date +%s)"

  (
    while :; do
      meminfo_value MemAvailable
      sleep 15
    done
  ) >"$memory_log" &
  monitor_pid=$!

  if ! current_container="$("${docker_command[@]}" run --detach \
    --name "$container_name" \
    --gpus all \
    --ipc host \
    --publish 127.0.0.1::8188 \
    --mount "type=bind,src=${model_dir},dst=/models,readonly" \
    --mount "type=bind,src=${candidate_dir},dst=/opt/ComfyUI/custom_nodes/MiniMaxH3_LatentUpscaler,readonly" \
    --mount "type=bind,src=${run_dir}/input,dst=/root/comfyui-input" \
    --mount "type=bind,src=${output_dir},dst=/root/comfyui-output" \
    --mount "type=bind,src=${run_dir}/temp,dst=/root/comfyui-temp" \
    --mount "type=bind,src=${run_dir}/user,dst=/root/comfyui-user" \
    "$image_ref" \
    python main.py \
    --listen 0.0.0.0 \
    --port 8188 \
    --input-directory /root/comfyui-input \
    --output-directory /root/comfyui-output \
    --temp-directory /root/comfyui-temp \
    --user-directory /root/comfyui-user \
    --extra-model-paths-config /opt/ComfyUI/extra_model_paths.yaml \
    --disable-auto-launch)"; then
    case_status='failed'
    case_error='container start failed'
    current_container=''
  fi

  if [[ "$case_status" == 'success' ]]; then
    host_port="$("${docker_command[@]}" port "$container_name" 8188/tcp | awk -F: 'END { print $NF }')"
    if [[ -z "$host_port" ]]; then
      case_status='failed'
      case_error='container port was not published'
    fi
  else
    host_port=''
  fi

  while [[ "$case_status" == 'success' ]]; do
    if curl --fail --silent --show-error "http://127.0.0.1:${host_port}/queue" >/dev/null 2>&1; then
      idle_available_kb="$(meminfo_value MemAvailable)"
      break
    fi
    if ! container_is_running "$container_name"; then
      case_status='failed'
      case_error='ComfyUI stopped before it became ready'
      "${docker_command[@]}" logs "$container_name" >&2 || true
      break
    fi
    sleep 1
  done

  if [[ "$case_status" == 'success' ]]; then
    jq -n --slurpfile graph "$graph_file" '{prompt: $graph[0]}' >"$request_file"
    if ! prompt_response="$(curl --fail --silent --show-error \
      --header 'Content-Type: application/json' \
      --data-binary "@${request_file}" \
      "http://127.0.0.1:${host_port}/prompt")"; then
      case_status='failed'
      case_error='ComfyUI rejected the prompt request'
    else
      prompt_id="$(jq -r '.prompt_id // empty' <<<"$prompt_response")"
      if [[ -z "$prompt_id" ]]; then
        case_status='failed'
        case_error='ComfyUI returned no prompt id'
      fi
    fi
  fi

  completed='false'
  while [[ "$case_status" == 'success' && "$completed" != 'true' ]]; do
    if history_response="$(curl --fail --silent --show-error \
      "http://127.0.0.1:${host_port}/history/${prompt_id}")"; then
      completed="$(jq -r --arg id "$prompt_id" '.[$id].status.completed // false' <<<"$history_response")"
      status_string="$(jq -r --arg id "$prompt_id" '.[$id].status.status_str // empty' <<<"$history_response")"
      if [[ "$status_string" == 'error' ]]; then
        case_status='failed'
        case_error='ComfyUI reported a generation error'
        printf '%s\n' "$history_response" >"$history_file"
        break
      fi
      if [[ "$completed" == 'true' ]]; then
        printf '%s\n' "$history_response" >"$history_file"
        break
      fi
    fi
    if ! container_is_running "$container_name"; then
      case_status='failed'
      case_error='ComfyUI stopped during generation'
      "${docker_command[@]}" logs "$container_name" >&2 || true
      break
    fi
    sleep 5
  done

  end_seconds="$(date +%s)"
  wall_seconds=$((end_seconds - start_seconds))
  kill "$monitor_pid" >/dev/null 2>&1 || true
  wait "$monitor_pid" 2>/dev/null || true
  peak_used_kb="$(awk -v total="$total_kb" '
    NR == 1 { minimum = $1 }
    $1 < minimum { minimum = $1 }
    END {
      if (NR == 0) print "n/a"
      else print total - minimum
    }
  ' "$memory_log")"

  relative_video=''
  if [[ "$case_status" == 'success' ]]; then
    relative_video="$(jq -r --arg id "$prompt_id" '
      first(
        .[$id].outputs
        | ..
        | objects
        | select(.type == "output" and (.filename // "") != "")
        | if (.subfolder // "") == ""
          then .filename
          else "\(.subfolder)/\(.filename)"
          end
      ) // empty
    ' "$history_file")"
    if [[ -z "$relative_video" || "$relative_video" == /* || "$relative_video" == *..* ]]; then
      case_status='failed'
      case_error='ComfyUI history did not name a safe output video'
    elif [[ ! -s "${output_dir}/${relative_video}" ]]; then
      case_status='failed'
      case_error='ComfyUI output video is missing or empty'
    fi
  fi

  if [[ "$case_status" == 'success' ]]; then
    container_video="/root/comfyui-output/${relative_video}"
    video_probe="$("${docker_command[@]}" exec "$container_name" \
      ffprobe -v error -select_streams v:0 \
      -show_entries stream=width,height,nb_frames -of csv=p=0 \
      "$container_video")"
    IFS=',' read -r video_width video_height video_frames <<<"$video_probe"
    case "$label" in
      *1920x1088)
        expected_width=1920
        expected_height=1088
        ;;
      *2560x1440)
        expected_width=2560
        expected_height=1440
        ;;
      *)
        expected_width='n/a'
        expected_height='n/a'
        ;;
    esac
    if [[ "$video_width" == "$expected_width" \
      && "$video_height" == "$expected_height" \
      && "$video_frames" == '124' ]]; then
      video_status='success'
    else
      case_status='failed'
      case_error='output video dimensions or frame count do not match the case'
    fi

    audio_codec="$("${docker_command[@]}" exec "$container_name" \
      ffprobe -v error -select_streams a:0 \
      -show_entries stream=codec_name -of default=noprint_wrappers=1:nokey=1 \
      "$container_video")"
    audio_duration="$("${docker_command[@]}" exec "$container_name" \
      ffprobe -v error -select_streams a:0 \
      -show_entries stream=duration -of default=noprint_wrappers=1:nokey=1 \
      "$container_video")"
    if [[ "$audio_codec" == 'aac' ]] \
      && awk -v duration="$audio_duration" 'BEGIN { exit !(duration > 0) }'; then
      audio_status='success'
    elif [[ "$case_status" == 'success' ]]; then
      case_status='failed'
      case_error='output lacks a non-empty AAC audio track'
    fi
  fi

  if [[ "$label" == two-pass-* ]] \
    && awk -v value="$audio_denoise" 'BEGIN { exit !(value > 0) }'; then
    garble_check='listen_for_garbling'
  elif [[ "$label" == two-pass-* ]]; then
    garble_check='listen_for_preservation'
  else
    garble_check='native_control'
  fi

  if [[ -n "$current_container" ]]; then
    "${docker_command[@]}" rm --force "$current_container" >/dev/null 2>&1 || true
    current_container=''
  fi

  printf 'RESULT label=%s status=%s wall_seconds=%s peak_used_kb=%s idle_available_kb=%s baseline_available_kb=%s total_kb=%s\n' \
    "$label" "$case_status" "$wall_seconds" "$peak_used_kb" \
    "$idle_available_kb" "$baseline_available_kb" "$total_kb"
  printf 'AUDIO label=%s status=%s codec=%s duration_seconds=%s garble_check=%s\n' \
    "$label" "$audio_status" "$audio_codec" "$audio_duration" "$garble_check"
  printf 'VIDEO label=%s status=%s width=%s height=%s frames=%s\n' \
    "$label" "$video_status" "$video_width" "$video_height" "$video_frames"
  if [[ -n "$relative_video" ]]; then
    printf 'OUTPUT label=%s file=%s\n' "$label" "${output_dir}/${relative_video}"
  fi
  if [[ -n "$case_error" ]]; then
    printf 'ERROR label=%s message=%s\n' "$label" "$case_error" >&2
  fi

  [[ "$case_status" == 'success' ]]
}

failure_count=0
for case_label in \
  native-1920x1088 \
  two-pass-1920x1088 \
  native-2560x1440 \
  two-pass-2560x1440; do
  if ! run_case "$case_label" "${graph_dir}/${case_label}.json"; then
    failure_count=$((failure_count + 1))
  fi
done

if ((failure_count > 0)); then
  exit 1
fi
REMOTE
remote_status=${PIPESTATUS[0]}
set -e

mkdir -p "$RESULTS_DIR/videos" "$RESULTS_DIR/graphs"
rsync --archive "${REMOTE_HOST}:${REMOTE_ROOT}/outputs/" "$RESULTS_DIR/videos/"
cp "$LOCAL_STAGE/manifest.json" "$RESULTS_DIR/manifest.json"
cp "$LOCAL_STAGE/graphs/"*.json "$RESULTS_DIR/graphs/"

printf 'Results: %s\n' "$RESULTS_DIR"
printf 'Target work directory: %s\n' "$REMOTE_ROOT"
exit "$remote_status"
