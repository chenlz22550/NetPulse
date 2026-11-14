# 开发、测试与发布

## 本地开发

Windows 兼容发行物必须用 Go 1.20.14。macOS 托盘构建需要 Xcode 命令行工具和 CGO；Linux 测试需要 GTK/AppIndicator 开发库（`libgtk-3-dev`、`libayatana-appindicator3-dev`）。

```sh
go1.20.14 run . -dir ./devdata -notray -nobrowser
node --test web/charts_test.cjs
python3 examples/plugins/email-notify/test_main.py
go1.20.14 test -count=1 ./...
go1.20.14 test -race -count=1 ./...
go1.20.14 vet ./...
```

前端为原生 JavaScript，不需要 npm 构建。不要对真实设备或通知渠道运行自动化测试。

## 打包构建

```sh
./build.sh            # macOS / Linux 交叉编译 Windows amd64，输出 dist/netPing.exe
./build_mac.sh        # macOS 通用二进制（arm64 + amd64）与 ad-hoc 签名
```

在 Windows PowerShell 上可运行 `build.ps1`。修改图标后重新生成 Windows 资源：`go1.20.14 run ./tools/genicon && go1.20.14 run github.com/tc-hib/go-winres@v0.3.3 make --arch amd64`。

## Wiki 构建

```sh
python3 -m pip install -r wiki/requirements.txt
python3 tools/build_wiki.py
```

输出 `dist/wiki/index.html`，样式、脚本与内容内联，可直接打开。源文件是 `wiki/` 下的 Markdown，技术参考同步读取 `docs/`。新增页面后更新生成器的导航列表。

## 流水线

提交和 Pull Request 执行 Go 测试、竞态检查、vet、前端图表测试、示例插件测试、Windows 兼容编译、Wiki 构建与全历史敏感信息扫描（Gitleaks）。`main` 分支推送构建并部署 GitHub Pages。打 `v*` 标签触发发布：先通过验证，再分别构建 Windows amd64 与 macOS 通用包、生成 `SHA256SUMS`，创建草稿 Release。审阅产物后手动发布。

## 兼容性

Go 1.20 已停止上游维护；它仅用于旧 Windows 的兼容构建。公开部署应优先使用受支持的操作系统，并评估工具链升级。macOS 包为 ad-hoc 签名，未做 Apple 公证。

## 贡献

阅读 [CONTRIBUTING.md](../CONTRIBUTING.md) 和 [SECURITY.md](../SECURITY.md)，提交可复现问题和范围明确的 PR。请使用虚构数据复现，不要上传现场配置、日志、截图中的设备信息或凭据。
