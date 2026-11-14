#!/usr/bin/env bash
# 编译 macOS 版本：通用二进制（Apple Silicon + Intel），打包为 netPing.app 并压缩为 zip。
# 需要 Xcode 命令行工具（托盘图标依赖 Cocoa，需开启 CGO）。
set -euo pipefail
cd "$(dirname "$0")"

GO=${GO:-go1.20.14}
command -v "$GO" >/dev/null 2>&1 || GO="$HOME/go/bin/go1.20.14"
command -v "$GO" >/dev/null 2>&1 || GO=go

VERSION=${VERSION:-$(sed -nE 's/^var version = "(.*)"/\1/p' main.go)}
VERSION=${VERSION#v}
[ -n "$VERSION" ] || { echo "无法从 main.go 读取版本号" >&2; exit 1; }
OUT=dist/mac
APP=$OUT/netPing.app
rm -rf "$OUT"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

echo "== 测试"
"$GO" test ./internal/... 2>&1 | awk '!/ld_classic/'

export CGO_ENABLED=1 MACOSX_DEPLOYMENT_TARGET=10.13
for arch in arm64 amd64; do
  carch=$arch
  [ "$arch" = amd64 ] && carch=x86_64
  echo "== 编译 darwin/$arch"
  GOOS=darwin GOARCH=$arch CC="clang -arch $carch" \
    "$GO" build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/netPing-$arch" . 2>&1 | awk '!/ld_classic/'
  [ -f "$OUT/netPing-$arch" ] || { echo "编译 $arch 失败"; exit 1; }
done
lipo -create -output "$APP/Contents/MacOS/netPing" "$OUT/netPing-arm64" "$OUT/netPing-amd64"
rm -f "$OUT/netPing-arm64" "$OUT/netPing-amd64"

echo "== 图标"
ICONSET=$OUT/netPing.iconset
mkdir -p "$ICONSET"
for s in 16 32 128 256; do
  sips -z $s $s winres/icon.png --out "$ICONSET/icon_${s}x${s}.png" >/dev/null
  d=$((s * 2))
  [ $d -le 256 ] && sips -z $d $d winres/icon.png --out "$ICONSET/icon_${s}x${s}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/netPing.icns"
rm -rf "$ICONSET"

cat > "$APP/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key><string>netPing</string>
	<key>CFBundleDisplayName</key><string>网脉 NetPulse</string>
	<key>CFBundleIdentifier</key><string>com.netping.app</string>
	<key>CFBundleExecutable</key><string>netPing</string>
	<key>CFBundleIconFile</key><string>netPing</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleVersion</key><string>${VERSION}</string>
	<key>CFBundleShortVersionString</key><string>${VERSION}</string>
	<key>LSMinimumSystemVersion</key><string>10.13</string>
	<key>LSUIElement</key><true/>
	<key>NSHighResolutionCapable</key><true/>
	<key>NSLocalNetworkUsageDescription</key><string>netPing 需要访问局域网，用于 ping 监控设备、扫描网段并读取 ARP 表（按 MAC 跟踪设备 IP）。</string>
</dict>
</plist>
EOF

echo "== 签名（ad-hoc）"
codesign --force --deep -s - "$APP"
codesign --verify --deep --strict "$APP"

ZIP="dist/netPing-macOS-${VERSION}.zip"
rm -f "$ZIP"
ditto -c -k --keepParent "$APP" "$ZIP"
echo "已生成 $APP 和 $ZIP"
lipo -info "$APP/Contents/MacOS/netPing"
