# 在 Windows 上编译（PowerShell）。需要安装 Go 1.20.x：
#   go install golang.org/dl/go1.20.14@latest; go1.20.14 download
param([string]$Go = "go1.20.14", [string]$Version = "1.0.0")
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot
$v = & $Go version
if ($v -notmatch "go1\.20") { throw "需要 Go 1.20.x（当前: $v），否则无法在 Server 2008 R2 上运行" }
New-Item -ItemType Directory -Force dist | Out-Null
& $Go test ./...
$env:GOOS = "windows"; $env:GOARCH = "amd64"; $env:CGO_ENABLED = "0"
& $Go build -trimpath -ldflags "-H windowsgui -s -w -X main.version=$Version" -o dist/netPing.exe .
Copy-Item docs/topology-fields.md, docs/plugin-development.md, schema/topology.schema.json dist/
Write-Host "已生成 dist/netPing.exe"
