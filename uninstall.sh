#!/bin/sh
# iknowledge 一键卸载(机器级):停服务、移除二进制与两处 kb-bootstrap skill。
# 项目级痕迹(.knowledge/ 与各接入配置)不在此清——脚本不知道你把哪些仓库接了库;
# 在项目里对 AI 说「卸载当前项目知识库」即可清干净(见结尾提示)。
set -eu

echo "==> 停掉所有运行中的 iknowledge serve"
pkill -f "iknowledge serve" 2>/dev/null || true

echo "==> 移除 kb-bootstrap skill(Claude Code)"
rm -rf "$HOME/.claude/skills/kb-bootstrap"

CODEX_DIR="${CODEX_HOME:-$HOME/.codex}"
if [ -d "$CODEX_DIR/skills/kb-bootstrap" ]; then
    echo "==> 移除 kb-bootstrap skill(Codex)"
    rm -rf "$CODEX_DIR/skills/kb-bootstrap"
fi

echo "==> 移除二进制"
if command -v go >/dev/null 2>&1; then
    rm -f "$(go env GOPATH)/bin/iknowledge"
else
    rm -f "$HOME/go/bin/iknowledge"
fi

echo ""
echo "机器级卸载完成。接入过的项目里各自还留有:"
echo "  .knowledge/(知识库本体,可能已随 git 提交——删前想清楚)"
echo "  .mcp.json / CLAUDE.md / AGENTS.md / .claude/settings.json 里的 knowledge 相关段"
echo "  ~/.codex/config.toml 里的 [mcp_servers.knowledge] 段"
echo "逐项目清理:在项目里对 AI 说「卸载当前项目知识库」(需 skill 尚未卸载时说,"
echo "或手动按上面清单移除)。"
