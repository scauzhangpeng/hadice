#!/usr/bin/env bash
#
# 单独打包 hadice-mcp（MCP stdio 桥）——纯新增脚本，不参与、不修改现有 Taskfile 流程。
#
# 产出：
#   dist/mcp/hadice-mcp_<version>_<os>_<arch>/hadice-mcp   分发目录（含 NOTICE / LICENSE）
#   dist/mcp/hadice-mcp_<version>_<os>_<arch>.zip          zip 包
#   可选：注入已存在的 bin/Hadice.app/Contents/MacOS/ 并重新 ad-hoc 签名
#
# 用法：
#   ./build/scripts/package_mcp.sh                    # 当前平台当前架构
#   ARCHS="amd64 arm64" ./build/scripts/package_mcp.sh # 多架构
#   INJECT_APP=1 ./build/scripts/package_mcp.sh        # 注入 bin/Hadice.app 并 ad-hoc 重签
#
# 说明：hadice-mcp 只是协议桥，不含设备能力，体积小；必须搭配运行中的 Hadice App 使用。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

# go 不在 PATH 时回退到本机安装位置
if command -v go >/dev/null 2>&1; then
  GO=go
elif [ -x /usr/local/go/bin/go ]; then
  GO=/usr/local/go/bin/go
else
  echo "错误：找不到 go 命令" >&2
  exit 1
fi

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
  arm64 | aarch64) HOST_ARCH=arm64 ;;
  x86_64 | amd64) HOST_ARCH=amd64 ;;
  *)
    echo "错误：未知架构 $(uname -m)" >&2
    exit 1
    ;;
esac
ARCHS="${ARCHS:-$HOST_ARCH}"

# 版本唯一来源：build/config.yml → info.version（两空格缩进）
VERSION="$(grep -E '^  version:' build/config.yml | head -1 | sed -E 's/.*"(.*)".*/\1/')"
if [ -z "$VERSION" ]; then
  echo "错误：无法从 build/config.yml 读取 info.version" >&2
  exit 1
fi

OUT_ROOT="dist/mcp"
mkdir -p "$OUT_ROOT"

echo "hadice-mcp 打包：version=$VERSION os=$OS archs=[$ARCHS]"

for ARCH in $ARCHS; do
  NAME="hadice-mcp_${VERSION}_${OS}_${ARCH}"
  DIR="$OUT_ROOT/$NAME"
  BIN="$DIR/hadice-mcp"
  [ "$OS" = "windows" ] && BIN="$DIR/hadice-mcp.exe"

  mkdir -p "$DIR"
  # hadice-mcp 传递依赖 wails/application（adb 侧发事件），darwin 必须开 cgo；
  # 跨架构 cgo 需要对应工具链，非本机架构时退化为纯 Go（本机场景不会走到）。
  if [ "$ARCH" = "$HOST_ARCH" ]; then
    CGO=1
  else
    CGO=0
    echo "  注意：跨架构 $ARCH 使用 CGO_ENABLED=0，若失败请在本机构建" >&2
  fi
  echo "  编译 $ARCH ..."
  GOOS="$OS" GOARCH="$ARCH" CGO_ENABLED="$CGO" "$GO" build \
    -trimpath \
    -ldflags "-s -w" \
    -o "$BIN" ./cmd/hadice-mcp

  # 第三方归属与许可证随包分发
  [ -f NOTICE ] && cp NOTICE "$DIR/NOTICE"
  [ -f LICENSE ] && cp LICENSE "$DIR/LICENSE"

  if command -v zip >/dev/null 2>&1; then
    rm -f "$OUT_ROOT/$NAME.zip"
    (cd "$OUT_ROOT" && zip -qr "$NAME.zip" "$NAME")
    echo "  -> $OUT_ROOT/$NAME.zip"
  else
    echo "  警告：未找到 zip 命令，仅保留目录 $DIR" >&2
  fi
done

# 可选：注入已打包好的 .app（macOS），让客户端配置指向包内固定路径
APP_BUNDLE="bin/Hadice.app"
if [ "${INJECT_APP:-0}" = "1" ]; then
  if [ -d "$APP_BUNDLE" ]; then
    for ARCH in $ARCHS; do
      if [ "$ARCH" = "$HOST_ARCH" ]; then
        cp "$OUT_ROOT/hadice-mcp_${VERSION}_${OS}_${ARCH}/hadice-mcp" \
          "$APP_BUNDLE/Contents/MacOS/hadice-mcp"
        chmod +x "$APP_BUNDLE/Contents/MacOS/hadice-mcp"
        echo "已注入 $APP_BUNDLE/Contents/MacOS/hadice-mcp"
      fi
    done
    if command -v codesign >/dev/null 2>&1; then
      codesign --force --deep --sign - "$APP_BUNDLE"
      echo "已重新 ad-hoc 签名（若已生成 DMG，需重新执行打包任务才会带上该文件）"
    fi
  else
    echo "跳过注入：$APP_BUNDLE 不存在（先执行打包任务）" >&2
  fi
fi

echo "完成。客户端配置示例（WorkBuddy / Codex 等 stdio 客户端）："
echo "  command: $(pwd)/$OUT_ROOT/hadice-mcp_${VERSION}_${OS}_${HOST_ARCH}/hadice-mcp"
