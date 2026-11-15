#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
repo_dir=$(pwd)
smoke_binary=$(mktemp /tmp/ColabStudio-smoke.XXXXXX)
trap 'rm -f "$smoke_binary"' EXIT
go build -tags smoke -o "$smoke_binary" .
COLABSTUDIO_SMOKE_VAULT=1 COLABSTUDIO_SMOKE_DIR="$repo_dir/evidence" "$smoke_binary"
cat evidence/ui-smoke.json
