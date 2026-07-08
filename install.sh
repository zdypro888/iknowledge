#!/bin/sh
# iknowledge 一键安装:优先下预编译二进制(免 Go 工具链),失败回退 go install。
# 装完后把 kb-bootstrap skill 装进 Claude Code 与 Codex。
# 之后在任何项目里对 AI 说"初始化当前项目知识库"即可,剩下的它自己做。
#
# 边界说明(与设计铁律二的关系):铁律二约束的是 iknowledge 二进制(对源码只读、
# 只写 .knowledge/)。本脚本不是那个二进制——它由用户显式执行,只写用户级 skill
# 目录(~/.claude/skills、$CODEX_HOME/skills),不碰任何代码仓库。
set -eu

REPO="zdypro888/iknowledge"
SKILL_URL="https://raw.githubusercontent.com/${REPO}/main/skills/kb-bootstrap/SKILL.md"

# ---- 探测平台 ----
os="$(uname -s)"
arch="$(uname -m)"
case "$os" in
    Darwin) os=darwin ;;
    Linux)  os=linux ;;
    MINGW*|MSYS*|CYGWIN*) os=windows ;;
    *) echo "错误: 不支持的系统 $os" >&2; exit 1 ;;
esac
case "$arch" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) echo "错误: 不支持的架构 $arch(仅支持 amd64/arm64)" >&2; exit 1 ;;
esac

# ---- 选择安装目录 ----
install_dir="${IKNOWLEDGE_BIN:-$HOME/.local/bin}"
mkdir -p "$install_dir"

# ---- 下载预编译二进制 ----
install_binary() {
    # 查最新 Release tag(GitHub API;无 tag 时返回空 → 回退源码构建)。
    latest_tag="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
        | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)"
    if [ -z "$latest_tag" ]; then
        return 1
    fi
    asset="iknowledge-${os}-${arch}.tar.gz"
    url="https://github.com/${REPO}/releases/download/${latest_tag}/${asset}"
    tmpdir="$(mktemp -d)"
    trap 'rm -rf "$tmpdir"' EXIT

    echo "==> 下载预编译二进制 ${latest_tag}/${asset}"
    if ! curl -fsSL "$url" -o "$tmpdir/$asset"; then
        echo "   下载失败(可能该平台尚无预编译包)" >&2
        return 1
    fi

    # 校验 sha256(若 checksums 文件存在且含本资产)。
    sums_url="https://github.com/${REPO}/releases/download/${latest_tag}/sha256sums.txt"
    if curl -fsSL "$sums_url" -o "$tmpdir/sha256sums.txt" 2>/dev/null; then
        awk -v a="$asset" '$2 == a { print }' "$tmpdir/sha256sums.txt" > "$tmpdir/$asset.sha256"
        if [ -s "$tmpdir/$asset.sha256" ]; then
            (cd "$tmpdir" && if command -v sha256sum >/dev/null 2>&1; then
                sha256sum -c "$asset.sha256" 2>/dev/null || { echo "   校验失败" >&2; return 1; }
            elif command -v shasum >/dev/null 2>&1; then
                shasum -a 256 -c "$asset.sha256" 2>/dev/null || { echo "   校验失败" >&2; return 1; }
            fi) || return 1
            echo "   sha256 校验通过"
        fi
    fi

    # 解压。tar 包内文件名是 iknowledge-{os}-{arch}(Windows 带 .exe)。
    tar xzf "$tmpdir/$asset" -C "$tmpdir"
    inner="iknowledge-${os}-${arch}"
    [ "$os" = "windows" ] && inner="$inner.exe"
    if [ ! -f "$tmpdir/$inner" ]; then
        echo "   解压后未找到 $inner" >&2
        return 1
    fi
    bin_name="iknowledge"
    [ "$os" = "windows" ] && bin_name="iknowledge.exe"
    mv "$tmpdir/$inner" "$install_dir/$bin_name"
    chmod +x "$install_dir/$bin_name"
    echo "   已安装 → $install_dir/$bin_name"
}

# ---- Go 源码构建(兜底) ----
install_from_source() {
    echo "==> 从源码构建(需要 Go 工具链)"
    if ! command -v go >/dev/null 2>&1; then
        echo "错误: 需要 Go 环境(https://go.dev/dl/),或等 Release 发布预编译包后重跑" >&2
        exit 1
    fi
    go install "github.com/${REPO}/cmd/iknowledge@latest"
    install_dir="$(go env GOPATH)/bin"
    echo "   已安装 → $install_dir/iknowledge"
}

# ---- 主安装逻辑 ----
BIN=""
if install_binary; then
    BIN="$install_dir/iknowledge"
elif [ -n "${IKNOWLEDGE_FORCE_SOURCE:-}" ]; then
    install_from_source
    BIN="$install_dir/iknowledge"
else
    echo "==> 预编译包不可用,回退源码构建"
    install_from_source
    BIN="$install_dir/iknowledge"
fi

# version 自检。
"$BIN" version

# ---- skill 安装 ----
fetch_skill() {
    dst="$1"
    mkdir -p "$dst"
    SELF_DIR="$(cd "$(dirname "$0")" 2>/dev/null && pwd || true)"
    if [ -n "$SELF_DIR" ] && [ -f "$SELF_DIR/skills/kb-bootstrap/SKILL.md" ]; then
        cp "$SELF_DIR/skills/kb-bootstrap/SKILL.md" "$dst/SKILL.md"
    else
        curl -fsSL "$SKILL_URL" -o "$dst/SKILL.md"
    fi
}

echo "==> 安装 kb-bootstrap skill(Claude Code)"
fetch_skill "$HOME/.claude/skills/kb-bootstrap"

CODEX_DIR="${CODEX_HOME:-$HOME/.codex}"
if [ -d "$CODEX_DIR" ]; then
    echo "==> 检测到 Codex,同步安装 skill"
    fetch_skill "$CODEX_DIR/skills/kb-bootstrap"
else
    echo "==> 未检测到 Codex($CODEX_DIR 不存在),跳过"
fi

# ---- PATH 可解析性(stdio 桥需要裸命令) ----
# stdio 桥由 MCP 客户端直接 spawn(GUI 启动的客户端 PATH 常没有 ~/.local/bin),
# 尽力软链进 /usr/local/bin 保证裸命令可解析;不行则明确提示。
if ! command -v iknowledge >/dev/null 2>&1; then
    if [ -w /usr/local/bin ] || ln -sf "$BIN" /usr/local/bin/iknowledge 2>/dev/null; then
        ln -sf "$BIN" /usr/local/bin/iknowledge 2>/dev/null || true
    fi
    if command -v iknowledge >/dev/null 2>&1 || [ -x /usr/local/bin/iknowledge ]; then
        echo "==> 已软链 /usr/local/bin/iknowledge(MCP stdio 桥需要裸命令可解析)"
    else
        echo "提示: iknowledge 不在 PATH——请执行(MCP stdio 桥依赖它):"
        echo "  sudo ln -sf \"$BIN\" /usr/local/bin/iknowledge"
        echo "(或让 AI 接入时在 .mcp.json 里用绝对路径 $BIN,skill 会自动这么做)"
    fi
fi

echo ""
echo "完成。现在进入任意项目,对 Claude Code 或 Codex 说:"
echo "  「初始化当前项目知识库」"
