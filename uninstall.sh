#!/bin/sh
# iknowledge 一键卸载(机器级):停服务、移除二进制与两处 kb-bootstrap skill。
# 项目级痕迹(.knowledge/ 与各接入配置)不在此清——脚本不知道你把哪些仓库接了库;
# 在项目里对 AI 说「卸载当前项目知识库」即可清干净(见结尾提示)。
set -eu

case "$(uname -s)" in
	MINGW*|MSYS*|CYGWIN*) platform="windows"; bin_name="iknowledge.exe" ;;
	Darwin) platform="darwin"; bin_name="iknowledge" ;;
	*) platform="unix"; bin_name="iknowledge" ;;
esac
install_dir="${IKNOWLEDGE_BIN:-$HOME/.local/bin}"
if [ -d "$install_dir" ]; then
	install_dir="$(cd "$install_dir" && pwd)"
fi

echo "==> 停掉所有运行中的 iknowledge serve"
if [ "$platform" = "windows" ]; then
	# Windows 不允许删除仍在运行的 .exe；Git Bash 通常也没有 pkill。
	MSYS2_ARG_CONV_EXCL='*' taskkill.exe /F /IM iknowledge.exe >/dev/null 2>&1 || true
else
	pkill -f "iknowledge serve" 2>/dev/null || true
fi

echo "==> 移除 kb-bootstrap skill(Claude Code)"
rm -rf "$HOME/.claude/skills/kb-bootstrap"

CODEX_DIR="${CODEX_HOME:-$HOME/.codex}"
if [ -d "$CODEX_DIR/skills/kb-bootstrap" ]; then
    echo "==> 移除 kb-bootstrap skill(Codex)"
    rm -rf "$CODEX_DIR/skills/kb-bootstrap"
fi

echo "==> 移除二进制(含 /usr/local/bin 软链)"
installed_bin="$install_dir/$bin_name"
# 只删与本次安装目标内容相同的别名；不能无条件抹掉用户另装的同名程序。
for candidate in /usr/local/bin/iknowledge /usr/local/bin/iknowledge.exe \
	"$HOME/.local/bin/iknowledge" "$HOME/.local/bin/iknowledge.exe" \
	"$HOME/go/bin/iknowledge" "$HOME/go/bin/iknowledge.exe"; do
	if [ "$candidate" != "$installed_bin" ] && [ -L "$candidate" ] && [ "$(readlink "$candidate" 2>/dev/null || true)" = "$installed_bin" ]; then
		rm -f "$candidate"
	elif [ "$candidate" != "$installed_bin" ] && [ -e "$candidate" ] && [ -e "$installed_bin" ] && cmp -s "$candidate" "$installed_bin"; then
		rm -f "$candidate"
	fi
done
rm -f "$installed_bin"

echo "==> 移除用户私有 auth/scout/semantic 状态"
if [ -n "${IKNOWLEDGE_STATE_HOME:-}" ]; then
	state_dir="$IKNOWLEDGE_STATE_HOME"
elif [ "$platform" = "windows" ]; then
	state_dir="${APPDATA:-$HOME/AppData/Roaming}/iknowledge/state"
elif [ "$platform" = "darwin" ]; then
	state_dir="$HOME/Library/Application Support/iknowledge/state"
else
	state_dir="${XDG_CONFIG_HOME:-$HOME/.config}/iknowledge/state"
fi
if [ -z "$state_dir" ] || [ "$state_dir" = "/" ] || [ "$state_dir" = "$HOME" ]; then
	echo "错误: 拒绝删除不安全的状态目录 $state_dir" >&2
	exit 1
fi
case "$state_dir" in
	/*|[A-Za-z]:[\\/]*) ;;
	*) echo "错误: 状态目录必须是绝对路径，拒绝删除 $state_dir" >&2; exit 1 ;;
esac
# override 目录可能还承载调用方自己的文件。逐仓只删本工具的 auth/scout/semantic；若发现崩溃
# 恢复 WAL 必须保留，避免“先 taskkill、再卸载”把半事务的唯一恢复依据抹掉。
if [ -L "$state_dir" ]; then
	echo "警告: 状态根目录是符号链接，已拒绝跟随并跳过: $state_dir" >&2
elif [ -L "$state_dir/repos" ]; then
	echo "警告: 状态 repos 是符号链接，已拒绝跟随并跳过: $state_dir/repos" >&2
elif [ -d "$state_dir/repos" ]; then
	for repo_state in "$state_dir"/repos/*; do
		if [ -L "$repo_state" ]; then
			echo "警告: 仓库状态是符号链接，已拒绝跟随并跳过: $repo_state" >&2
			continue
		fi
		[ -d "$repo_state" ] || continue
		rm -f "$repo_state/auth-token" "$repo_state/local-identity" "$repo_state/scout-trust-v1" "$repo_state/semantic-config-v1.json"
		if [ -e "$repo_state/transaction-v1.json" ] || [ -e "$repo_state/transaction-v1.commit" ]; then
			echo "   保留待恢复事务状态: $repo_state"
			continue
		fi
		rmdir "$repo_state" 2>/dev/null || true
	done
	rmdir "$state_dir/repos" 2>/dev/null || true
fi
rmdir "$state_dir" 2>/dev/null || true

echo ""
echo "机器级卸载完成。接入过的项目里各自还留有:"
echo "  .knowledge/(知识库本体,可能已随 git 提交——删前想清楚)"
echo "  .mcp.json / CLAUDE.md / AGENTS.md / .claude/settings.json 里的 knowledge 相关段"
echo "  ~/.codex/config.toml 里的 [mcp_servers.knowledge] 段"
echo "逐项目清理:在项目里对 AI 说「卸载当前项目知识库」(需 skill 尚未卸载时说,"
echo "或手动按上面清单移除)。"
