#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
go run github.com/egoist/mygo/cmd/mygo build -skip-notarize "$@"
