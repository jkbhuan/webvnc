#!/usr/bin/env bash
#
# 构建 webvnc：先编译前端，再把前端产物嵌入 Go 二进制。
#
# 这是 build.ps1 的 Linux 对应物，参数一一对应。构建顺序不能颠倒：
# go:embed 在编译期读取 frontend/dist，所以必须先生成 dist，
# 否则 go build 会直接报错。
#
# 用法:
#   ./build.sh                              完整构建，输出 webvnc
#   ./build.sh -o webvnc-dev                跳过 npm install，输出到指定文件名
#   ./build.sh --skip-frontend              只重新编译 Go（前端产物没有改动时）
#   ./build.sh --version 0.2.0              注入版本号
#
set -euo pipefail

cd "$(dirname "$(readlink -f "$0")")"

output='webvnc'
version='0.1.0'
skip_install=0
skip_frontend=0

while [ $# -gt 0 ]; do
    case "$1" in
        -o|--output)     output="$2"; shift 2 ;;
        --version)       version="$2"; shift 2 ;;
        --skip-install)  skip_install=1; shift ;;
        --skip-frontend) skip_frontend=1; shift ;;
        -h|--help)       sed -n '3,15p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) echo "未知参数: $1（--help 看用法）" >&2; exit 2 ;;
    esac
done

step() { printf '\n==> %s\n' "$1"; }

require() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "找不到命令 '$1'。$2" >&2
        exit 1
    fi
}

# --- 环境检查 -------------------------------------------------------------
require go '请先安装 Go 1.22 或更高版本: https://go.dev/dl/'
if [ "$skip_frontend" -eq 0 ]; then
    require npm '请先安装 Node.js 18 或更高版本: https://nodejs.org/'
fi

# 抓屏和输入注入都通过 X11。这里只提示，不拦：装了 Xvfb 的 CI 机器
# 在构建阶段并不需要 DISPLAY。
if [ -z "${DISPLAY:-}" ]; then
    echo "提示: 当前环境没有 DISPLAY。构建不受影响，但运行 webvnc 需要 X 服务器" >&2
    echo "      （没有桌面会话时可以用 Xvfb :99 起一个）。" >&2
fi

# --- 前端 -----------------------------------------------------------------
if [ "$skip_frontend" -eq 0 ]; then
    cd frontend
    if [ "$skip_install" -eq 0 ]; then
        step '安装前端依赖 (npm install)'
        npm install --no-fund --no-audit
    fi
    step '构建前端 (npm run build)'
    # 走 build 而不是 build:only：它会先跑一遍 vue-tsc 类型检查，
    # 这里保留它以便尽早发现类型错误。
    npm run build
    cd ..
fi

if [ ! -f frontend/dist/index.html ]; then
    echo '找不到 frontend/dist/index.html。请先执行 npm run build，或去掉 --skip-frontend 重新构建。' >&2
    exit 1
fi

# --- 后端 -----------------------------------------------------------------
step "编译 Go 二进制 ($output)"
# 纯 Go：X11 走 github.com/jezek/xgb 的协议实现，没有任何 cgo 依赖，
# 所以产物仍然是单个静态可执行文件。
export CGO_ENABLED=0

go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$output" .
if [ ! -x "$output" ]; then
    echo 'go build 没有产出可执行文件' >&2
    exit 1
fi

# --- 结果 -----------------------------------------------------------------
size=$(du -h "$output" | cut -f1)
# -o 可以给绝对路径，这时前面再拼一次 pwd 只会拼出一个不存在的路径。
case "$output" in
    /*) where="$output" ;;
    *)  where="$(pwd)/$output" ;;
esac
printf '\n构建完成: %s  (%s)\n\n' "$where" "$size"
cat <<'EOF'
运行方式:
  ./webvnc                                  # 监听 8339，口令随机生成
  ./webvnc -password 你的口令 -fps 20        # 指定口令和帧率
  ./webvnc -codec jpeg                      # 强制帧内 JPEG（CPU 占用最低）

提示: 该二进制的画面通道依赖 X11（通过 XWayland 也能注入输入，但抓屏只覆盖
      X11 程序）。Wayland 原生会话请见 README 的"平台支持"一节。
EOF
