#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

targets=("amd64" "arm64")
if [ "$#" -gt 0 ]; then
  targets=("$@")
fi

for arch in "${targets[@]}"; do
  out="build/linux-${arch}/colabstudio-server"
  mkdir -p "$(dirname "$out")"
  echo "正在交叉编译 ${out} (CGO_ENABLED=0 GOOS=linux GOARCH=${arch} -tags headless)..."
  CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" go build \
    -tags headless \
    -trimpath \
    -ldflags="-s -w" \
    -o "$out" \
    .
done

echo "Linux 服务端二进制编译完成。"
