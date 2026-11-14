#!/usr/bin/env bash
# 交叉编译 Windows 版本（在 macOS / Linux 上执行）。
# Windows Server 2008 R2 / Windows 7 只能运行 Go 1.20 及以下编译的程序，必须使用 go1.20.14：
#   go install golang.org/dl/go1.20.14@latest && go1.20.14 download
set -euo pipefail
cd "$(dirname "$0")"
GO=${GO:-go1.20.14}
VERSION=${VERSION:-$(sed -nE 's/^var version = "(.*)"/\1/p' main.go)}
VERSION=${VERSION#v}
[ -n "$VERSION" ] || { echo "无法从 main.go 读取版本号" >&2; exit 1; }
if ! command -v "$GO" >/dev/null 2>&1; then
  echo "未找到 $GO，请安装 Go 1.20.14 后重试" >&2
  exit 1
fi
if ! "$GO" version | grep -q "go1.20"; then
  echo "需要 Go 1.20.x（当前: $("$GO" version)），否则编译出的程序无法在 Server 2008 R2 上运行" >&2
  exit 1
fi
mkdir -p dist
"$GO" test ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 "$GO" build -trimpath \
  -ldflags "-H windowsgui -s -w -X main.version=$VERSION" -o dist/netPing.exe .
cp docs/topology-fields.md docs/plugin-development.md dist/
cp schema/topology.schema.json dist/
echo "已生成 dist/netPing.exe"
