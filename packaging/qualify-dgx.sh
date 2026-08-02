#!/bin/sh
set -eu

usage() {
  echo "usage: $0 MANAGER_URL RECIPE_ID [RECEIPT_DIR] [--remove]" >&2
  exit 2
}

# --remove is accepted in any position so omitting the optional RECEIPT_DIR
# cannot silently swallow the flag into a directory name.
remove_after=false
positional_count=0
for argument in "$@"; do
  case "$argument" in
    --remove) remove_after=true ;;
    --*) usage ;;
    *)
      positional_count=$((positional_count + 1))
      eval "positional_${positional_count}=\$argument"
      ;;
  esac
done
if [ "$positional_count" -lt 2 ] || [ "$positional_count" -gt 3 ]; then usage; fi
# shellcheck disable=SC2154 # positional_N are assigned via eval above
manager_url=${positional_1%/}
# shellcheck disable=SC2154
recipe_id=$positional_2
receipt_dir=${positional_3:-"$PWD/qualification-receipts"}

case "$manager_url" in
  http://*|https://*) ;;
  *) echo "manager URL must start with http:// or https://" >&2; exit 2 ;;
esac
case "$recipe_id" in
  *[!a-z0-9-]*|'') echo "recipe ID contains unsupported characters" >&2; exit 2 ;;
esac
command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 1; }

pairing_file=${BASEMENT_PAIRING_TOKEN_FILE:-/var/lib/basement/pairing-token}
[ -r "$pairing_file" ] || { echo "pairing token is not readable at $pairing_file" >&2; exit 1; }
umask 077
mkdir -p "$receipt_dir"
timestamp=$(date -u +%Y%m%dT%H%M%SZ)
receipt_file="$receipt_dir/${timestamp}-${recipe_id}.jsonl"
temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/basement-qualify.XXXXXX")
cookie_file="$temporary_dir/cookies.txt"

cleanup() {
  find "$temporary_dir" -type f -delete 2>/dev/null || true
  rmdir "$temporary_dir" 2>/dev/null || true
}
trap cleanup EXIT HUP INT TERM

record_json() {
  event=$1
  source_file=$2
  python3 - "$receipt_file" "$event" "$source_file" <<'PY'
import datetime, json, pathlib, sys
receipt, event, source = sys.argv[1:]
with open(source, encoding="utf-8") as handle:
    detail = json.load(handle)
entry = {"at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "event": event, "detail": detail}
with open(receipt, "a", encoding="utf-8") as handle:
    handle.write(json.dumps(entry, separators=(",", ":")) + "\n")
PY
}

record_value() {
  event=$1
  value=$2
  python3 - "$receipt_file" "$event" "$value" <<'PY'
import datetime, json, sys
receipt, event, value = sys.argv[1:]
entry = {"at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "event": event, "detail": value}
with open(receipt, "a", encoding="utf-8") as handle:
    handle.write(json.dumps(entry, separators=(",", ":")) + "\n")
PY
}

json_field() {
  source_file=$1
  field=$2
  python3 - "$source_file" "$field" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as handle:
    value = json.load(handle)
for part in sys.argv[2].split("."):
    value = value[part]
if isinstance(value, bool):
    print("true" if value else "false")
else:
    print(value)
PY
}

authenticated_get() {
  path=$1
  output=$2
  curl --fail --silent --show-error --cookie "$cookie_file" "$manager_url$path" --output "$output"
}

create_job() {
  method=$1
  path=$2
  body=$3
  output=$4
  key="qualification-${timestamp}-$(date +%s)-$$"
  curl --fail --silent --show-error --request "$method" --cookie "$cookie_file" \
    --header "Origin: $manager_url" --header "X-CSRF-Token: $csrf_token" \
    --header "Idempotency-Key: $key" --header "Content-Type: application/json" \
    --data "$body" "$manager_url$path" --output "$output"
}

wait_for_job() {
  job_id=$1
  expected_state=$2
  event=$3
  deadline=$(( $(date +%s) + 21600 ))
  job_file="$temporary_dir/job-${job_id}.json"
  while [ "$(date +%s)" -lt "$deadline" ]; do
    authenticated_get "/api/v1/jobs/$job_id" "$job_file"
    state=$(json_field "$job_file" state)
    case "$state" in
      ready|stopped|removed)
        record_json "$event" "$job_file"
        [ "$state" = "$expected_state" ] || { echo "job $job_id reached $state, expected $expected_state" >&2; exit 1; }
        if [ "$expected_state" = "ready" ]; then
          python3 - "$job_file" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as handle:
    job = json.load(handle)
if not any(step.get("receipt", {}).get("response_non_empty") is True for step in job.get("steps", [])):
    raise SystemExit("ready job lacks a non-empty inference receipt")
PY
        fi
        return
        ;;
      failed|cancelled)
        record_json "$event" "$job_file"
        echo "job $job_id ended in $state" >&2
        exit 1
        ;;
    esac
    sleep 5
  done
  echo "job $job_id did not finish within six hours" >&2
  exit 1
}

health_file="$temporary_dir/health.json"
curl --fail --silent --show-error "$manager_url/healthz" --output "$health_file"
record_json health "$health_file"

pair_request="$temporary_dir/pair-request.json"
pair_response="$temporary_dir/pair-response.json"
pairing_token=$(tr -d '\r\n' < "$pairing_file")
printf '%s' "$pairing_token" | python3 -c 'import json,sys; print(json.dumps({"token":sys.stdin.read()}))' > "$pair_request"
unset pairing_token
curl --fail --silent --show-error --cookie-jar "$cookie_file" --header "Origin: $manager_url" \
  --header "Content-Type: application/json" --data-binary "@$pair_request" \
  "$manager_url/api/v1/auth/pair" --output "$pair_response"
csrf_token=$(json_field "$pair_response" csrf_token)
find "$temporary_dir" -name 'pair-*.json' -type f -delete
record_value paired "local pairing succeeded; credential omitted"

system_file="$temporary_dir/system.json"
recipes_file="$temporary_dir/recipes.json"
preflight_file="$temporary_dir/preflight.json"
authenticated_get /api/v1/system "$system_file"
record_json system "$system_file"
authenticated_get /api/v1/recipes "$recipes_file"
python3 - "$recipes_file" "$recipe_id" "$temporary_dir/recipe.json" <<'PY'
import json, sys
source, recipe_id, target = sys.argv[1:]
with open(source, encoding="utf-8") as handle:
    recipes = json.load(handle)
match = next((item for item in recipes if item.get("id") == recipe_id), None)
if match is None:
    raise SystemExit(f"recipe {recipe_id} is not embedded in this manager")
with open(target, "w", encoding="utf-8") as handle:
    json.dump(match, handle)
PY
record_json recipe "$temporary_dir/recipe.json"
authenticated_get "/api/v1/preflight?recipe_id=$recipe_id" "$preflight_file"
record_json preflight "$preflight_file"
[ "$(json_field "$preflight_file" ready)" = "true" ] || { echo "preflight failed; see $receipt_file" >&2; exit 1; }

response_file="$temporary_dir/response.json"
create_job POST "/api/v1/models/$recipe_id/install" '{"confirmed":true,"accept_licence":true}' "$response_file"
install_job=$(json_field "$response_file" job.id)
wait_for_job "$install_job" ready install

create_job POST "/api/v1/models/$recipe_id/stop" '{}' "$response_file"
stop_job=$(json_field "$response_file" job.id)
wait_for_job "$stop_job" stopped stop

create_job POST "/api/v1/models/$recipe_id/start" '{}' "$response_file"
start_job=$(json_field "$response_file" job.id)
wait_for_job "$start_job" ready start

create_job POST "/api/v1/models/$recipe_id/smoke-test" '{}' "$response_file"
smoke_job=$(json_field "$response_file" job.id)
wait_for_job "$smoke_job" ready smoke_test

diagnostics_file="$receipt_dir/${timestamp}-${recipe_id}-diagnostics.json"
authenticated_get /api/v1/diagnostics "$diagnostics_file"
python3 - "$diagnostics_file" <<'PY'
import json, re, sys
with open(sys.argv[1], encoding="utf-8") as handle:
    bundle = json.load(handle)
text = json.dumps(bundle)
if bundle.get("redacted") is not True or bundle.get("format") != "basement-diagnostics-v1":
    raise SystemExit("diagnostic bundle is not marked as the redacted v1 format")
if re.search(r"hf_[A-Za-z0-9]{12,}", text):
    raise SystemExit("diagnostic bundle contains a likely Hugging Face token")
PY
record_value diagnostics "$diagnostics_file"

if [ "$remove_after" = true ]; then
  reclaim_bytes=$(json_field "$temporary_dir/recipe.json" artifact_bytes)
  create_job DELETE "/api/v1/models/$recipe_id" "{\"remove_artifacts\":true,\"expected_reclaim_bytes\":$reclaim_bytes}" "$response_file"
  remove_job=$(json_field "$response_file" job.id)
  wait_for_job "$remove_job" removed remove
fi

record_value result "PASS on real manager responses; recipe trust metadata was not changed"
echo "$receipt_file"
