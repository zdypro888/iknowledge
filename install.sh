#!/bin/sh
# iknowledge 一键安装:装二进制 + 把 kb-bootstrap skill 装进 Claude Code 与 Codex。
# 之后在任何项目里对 AI 说"初始化当前项目知识库"即可,剩下的它自己做。
#
# 边界说明(与设计铁律二的关系):铁律二约束的是 iknowledge 二进制(对源码只读、
# 只写 .knowledge/)。本脚本不是那个二进制——它由用户显式执行,只写用户级 skill
# 目录(~/.claude/skills、$CODEX_HOME/skills),不碰任何代码仓库。
set -eu

SKILL_URL="https://raw.githubusercontent.com/zdypro888/iknowledge/main/skills/kb-bootstrap/SKILL.md"

echo "==> 安装 iknowledge 二进制"
if ! command -v go >/dev/null 2>&1; then
    echo "错误: 需要 Go 环境(https://go.dev/dl/),装好后重跑本脚本" >&2
    exit 1
fi
go install github.com/zdypro888/iknowledge/cmd/iknowledge@latest
BIN="$(go env GOPATH)/bin/iknowledge"
"$BIN" version

# skill 内容:仓库内执行用本地文件,curl | sh 场景从 main 拉取。
SELF_DIR="$(cd "$(dirname "$0")" 2>/dev/null && pwd || true)"
fetch_skill() {
    dst="$1"
    mkdir -p "$dst"
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

# stdio 桥由 MCP 客户端直接 spawn(GUI 启动的客户端 PATH 常没有 ~/go/bin),
# 尽力软链进 /usr/local/bin 保证裸命令可解析;不行则明确提示。
if ! command -v iknowledge >/dev/null 2>&1; then
    if [ -w /usr/local/bin ] || ln -sf "$BIN" /usr/local/bin/iknowledge 2>/dev/null; then
        ln -sf "$BIN" /usr/local/bin/iknowledge 2>/dev/null || true
    fi
    if command -v iknowledge >/dev/null 2>&1 || [ -x /usr/local/bin/iknowledge ]; then
        echo "==> 已软链 /usr/local/bin/iknowledge(MCP 客户端 spawn stdio 桥需要裸命令可解析)"
    else
        echo "提示: iknowledge 不在 PATH——请执行(MCP stdio 桥依赖它):"
        echo "  sudo ln -sf \"$BIN\" /usr/local/bin/iknowledge"
        echo "(或让 AI 接入时在 .mcp.json 里用绝对路径 $BIN,skill 会自动这么做)"
    fi
fi

echo ""
echo "完成。现在进入任意项目,对 Claude Code 或 Codex 说:"
echo "  「初始化当前项目知识库」"
