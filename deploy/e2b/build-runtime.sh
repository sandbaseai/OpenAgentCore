#!/usr/bin/env bash
set -euo pipefail

# Reuse the selected revision's native builders and combined Runtime Dockerfile.
source_root="${1:?Usage: build-runtime.sh SOURCE_ROOT OUTPUT_ROOT}"
output_root="${2:?Usage: build-runtime.sh SOURCE_ROOT OUTPUT_ROOT}"
[[ "$source_root" == /* && "$output_root" == /* ]]
[[ "$(uname -s):$(uname -m)" == Linux:x86_64 ]]
cd "$source_root"
mkdir -p "$output_root"
bash scripts/prepare-release-runtimes.sh
inputs="$HOME/.oac/build/release-inputs/inputs.json"
AGENTS_RUNTIME_CODEX_PACKAGE="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["codex"])' "$inputs")"
MCODE_HARNESS_BUILD_DIR="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["mcode"])' "$inputs")"
CLAUDE_SDK_BUILD_DIR="$output_root/claude-sdk"
export AGENTS_RUNTIME_CODEX_PACKAGE MCODE_HARNESS_BUILD_DIR CLAUDE_SDK_BUILD_DIR
bash scripts/build-claude-sdk-runtime.sh
revision="$(git rev-parse HEAD)"
for harness in codex claude mcode; do
  builder="scripts/build-$harness-runtime.sh"
  if [[ "$harness" == codex ]]; then builder=scripts/build-agents-runtime.sh; fi
  AGENTS_RUNTIME_BUILD_DIR="$output_root/$harness" bash "$builder"
  docker build --platform linux/amd64 --tag "oac-e2b-$harness:$revision" "$output_root/$harness"
  image="$(docker image inspect --format '{{.Id}}' "oac-e2b-$harness:$revision")"
  python3 scripts/core-distribution-manifest.py verify-runtime \
    "$image" "$output_root/$harness/oac-daemon" "$source_root"
done
# The maintained multi-stage Dockerfile advertises and checks all three Harnesses.
mkdir -p "$output_root/combined"
cp deploy/distribution/Runtime.Dockerfile "$output_root/combined/Dockerfile"
docker build --platform linux/amd64 \
  --build-arg "CODEX_IMAGE=oac-e2b-codex:$revision" \
  --build-arg "CLAUDE_IMAGE=oac-e2b-claude:$revision" \
  --build-arg "MCODE_IMAGE=oac-e2b-mcode:$revision" \
  --label "org.opencontainers.image.revision=$revision" \
  --tag "oac-e2b-runtime:$revision" "$output_root/combined"
image="$(docker image inspect --format '{{.Id}}' "oac-e2b-runtime:$revision")"
python3 scripts/core-distribution-manifest.py verify-runtime \
  "$image" "$output_root/mcode/oac-daemon" "$source_root"
printf '%s\n' "$image" > "$output_root/runtime-image.id"
