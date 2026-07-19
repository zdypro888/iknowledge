#!/bin/sh
# iknowledge 一键卸载(机器级):停服务、移除二进制与两处 kb-bootstrap skill。
# 项目级痕迹(.knowledge/ 与各接入配置)不在此清——脚本不知道你把哪些仓库接了库;
# 在项目里对 AI 说「卸载当前项目知识库」即可清干净(见结尾提示)。
set -eu

case "$(uname -s)" in
	MINGW*|MSYS*|CYGWIN*) platform="windows"; bin_name="iknowledge.exe" ;;
	Darwin) platform="darwin"; bin_name="iknowledge" ;;
	Linux) platform="linux"; bin_name="iknowledge" ;;
	*) platform="unix"; bin_name="iknowledge" ;;
esac
install_dir_requested="${IKNOWLEDGE_BIN:-$HOME/.local/bin}"
install_dir="$install_dir_requested"
install_dir_logical="$install_dir_requested"
if [ -d "$install_dir_requested" ]; then
	install_dir_logical="$(cd "$install_dir_requested" && pwd -L)"
	install_dir="$(cd -P "$install_dir_requested" && pwd)"
fi
case "$install_dir" in
	/*|[A-Za-z]:[\\/]*) ;;
	*) echo "错误: 安装目录不存在时 IKNOWLEDGE_BIN 必须是绝对路径" >&2; exit 1 ;;
esac

installed_bin="$install_dir/$bin_name"
installed_bin_logical="$install_dir_logical/$bin_name"

uninstall_manifest="$(mktemp "${TMPDIR:-/tmp}/iknowledge-uninstall.XXXXXX")"
cleanup_uninstall() {
	rm -f "$uninstall_manifest"
}
trap cleanup_uninstall EXIT

symlink_points_to_installed_bin() {
	uninstall_link_path="$1"
	[ -L "$uninstall_link_path" ] || return 1
	uninstall_link_value="$(readlink "$uninstall_link_path" 2>/dev/null)" || return 1
	case "$uninstall_link_value" in
		/*) uninstall_link_absolute="$uninstall_link_value" ;;
		*) uninstall_link_absolute="$(cd -P "$(dirname "$uninstall_link_path")" 2>/dev/null && pwd)/$uninstall_link_value" ;;
	esac
	uninstall_link_parent="$(dirname "$uninstall_link_absolute")"
	[ -d "$uninstall_link_parent" ] || return 1
	uninstall_link_absolute="$(cd -P "$uninstall_link_parent" 2>/dev/null && pwd)/$(basename "$uninstall_link_absolute")"
	[ "$uninstall_link_absolute" = "$installed_bin" ]
}

# argv 只做候选筛选；只停止当前 UID 且 executable identity 指向受控路径的 serve。
# 禁止使用 taskkill.exe /F /IM iknowledge.exe 或 pkill -f "iknowledge serve"：
# 按名字匹配会误杀其他目录、其他用户或恰好同名的程序。
resolve_controlled_executable() {
	uninstall_resolve_path="$1"
	uninstall_resolve_hops=0
	while [ -L "$uninstall_resolve_path" ]; do
		[ "$uninstall_resolve_hops" -lt 16 ] || return 1
		uninstall_resolve_link="$(readlink "$uninstall_resolve_path" 2>/dev/null)" || return 1
		uninstall_resolve_parent="$(cd -P "$(dirname "$uninstall_resolve_path")" 2>/dev/null && pwd)" || return 1
		case "$uninstall_resolve_link" in
			/*) uninstall_resolve_path="$uninstall_resolve_link" ;;
			*) uninstall_resolve_path="$uninstall_resolve_parent/$uninstall_resolve_link" ;;
		esac
		uninstall_resolve_hops=$((uninstall_resolve_hops + 1))
	done
	uninstall_resolve_parent="$(cd -P "$(dirname "$uninstall_resolve_path")" 2>/dev/null && pwd)" || return 1
	printf '%s/%s\n' "$uninstall_resolve_parent" "$(basename "$uninstall_resolve_path")"
}

pid_executable_path() {
	uninstall_executable_pid="$1"
	uninstall_executable_target="$2"
	case "$platform" in
		linux)
			readlink "/proc/$uninstall_executable_pid/exe" 2>/dev/null
			;;
		darwin)
			if [ -x /usr/sbin/lsof ]; then
				uninstall_lsof_command=/usr/sbin/lsof
			elif command -v lsof >/dev/null 2>&1; then
				uninstall_lsof_command="$(command -v lsof)"
			else
				return 1
			fi
			uninstall_lsof_output="$("$uninstall_lsof_command" -a -p "$uninstall_executable_pid" -d txt -Fn 2>/dev/null)" || return 1
			uninstall_executable_path=""
			uninstall_executable_target_path="$(resolve_controlled_executable "$uninstall_executable_target")" || return 1
			while IFS= read -r uninstall_lsof_line; do
				case "$uninstall_lsof_line" in
					n/*)
						uninstall_lsof_path="${uninstall_lsof_line#n}"
						case "$uninstall_lsof_path" in *" (deleted)") uninstall_lsof_path="${uninstall_lsof_path% (deleted)}" ;; esac
						[ -n "$uninstall_executable_path" ] || uninstall_executable_path="$uninstall_lsof_path"
						uninstall_lsof_resolved="$(resolve_controlled_executable "$uninstall_lsof_path" 2>/dev/null || true)"
						if [ "$uninstall_lsof_resolved" = "$uninstall_executable_target_path" ]; then
							printf '%s\n' "$uninstall_lsof_path"
							return 0
						fi
						;;
				esac
			done <<EOF
$uninstall_lsof_output
EOF
			[ -n "$uninstall_executable_path" ] || return 1
			printf '%s\n' "$uninstall_executable_path"
			;;
		*) return 1 ;;
	esac
}

serve_command_candidate() {
	uninstall_candidate_command="$1"
	uninstall_candidate_target="$2"
	case "$uninstall_candidate_command" in
		"$uninstall_candidate_target serve"|"$uninstall_candidate_target serve "*|\
		"iknowledge serve"|"iknowledge serve "*|\
		"iknowledge.exe serve"|"iknowledge.exe serve "*) return 0 ;;
	esac
	return 1
}

# 返回 0=身份确认；1=进程消失/其他安装；2=候选仍在但无法证明身份。
pid_is_exact_serve() {
	uninstall_check_pid="$1"
	uninstall_check_target="$2"
	uninstall_process_line="$(ps -p "$uninstall_check_pid" -o uid=,args= 2>/dev/null)" || return 1
	uninstall_owner_uid=""
	uninstall_command=""
	read -r uninstall_owner_uid uninstall_command <<EOF
$uninstall_process_line
EOF
	[ "$uninstall_owner_uid" = "$uninstall_current_uid" ] || return 1
	serve_command_candidate "$uninstall_command" "$uninstall_check_target" || return 1
	if ! uninstall_executable_path="$(pid_executable_path "$uninstall_check_pid" "$uninstall_check_target")"; then
		echo "错误: 发现当前 UID 的 iknowledge serve 候选 PID ${uninstall_check_pid}，但无法证明其 executable identity；拒绝继续，请手动确认并停止后重跑" >&2
		return 2
	fi
	case "$uninstall_executable_path" in
		*" (deleted)") uninstall_executable_path="${uninstall_executable_path% (deleted)}" ;;
	esac
	case "$uninstall_executable_path" in
		/*) ;;
		*) echo "错误: PID $uninstall_check_pid 的 executable identity 不是绝对路径；拒绝继续" >&2; return 2 ;;
	esac
	if ! uninstall_executable_path="$(resolve_controlled_executable "$uninstall_executable_path")" || ! uninstall_controlled_path="$(resolve_controlled_executable "$uninstall_check_target")"; then
		echo "错误: 无法规范化 PID $uninstall_check_pid 或受控安装路径的 executable identity；拒绝继续" >&2
		return 2
	fi
	[ "$uninstall_executable_path" = "$uninstall_controlled_path" ] || return 1
	return 0
}

exact_serve_pids() {
	uninstall_scan_target="$1"
	if ! uninstall_process_list="$(ps -axo uid=,pid=,args= 2>/dev/null)"; then
		return 1
	fi
	uninstall_scan_pids=""
	uninstall_scan_failed=0
	while read -r uninstall_owner_uid uninstall_pid uninstall_command; do
		[ "$uninstall_owner_uid" = "$uninstall_current_uid" ] || continue
		serve_command_candidate "$uninstall_command" "$uninstall_scan_target" || continue
		uninstall_pid_state=0
		pid_is_exact_serve "$uninstall_pid" "$uninstall_scan_target" || uninstall_pid_state=$?
		case "$uninstall_pid_state" in
			0) uninstall_scan_pids="$uninstall_scan_pids $uninstall_pid" ;;
			2) uninstall_scan_failed=1 ;;
		esac
	done <<EOF
$uninstall_process_list
EOF
	[ "$uninstall_scan_failed" -eq 0 ] || return 1
	printf '%s\n' "$uninstall_scan_pids" | sed 's/^[[:space:]]*//'
}

stop_exact_serves() {
	uninstall_stop_target="$1"
	if ! command -v ps >/dev/null 2>&1; then
		echo "错误: 无法安全检查 serve；请手动停止由 $uninstall_stop_target 启动的服务后重跑" >&2
		return 1
	fi
	if ! uninstall_pids="$(exact_serve_pids "$uninstall_stop_target")"; then
		echo "错误: 无法读取进程表以检查由 $uninstall_stop_target 启动的 serve" >&2
		return 1
	fi
	[ -n "$uninstall_pids" ] || return 0
	echo "==> 停止由受控安装路径启动的 serve: $uninstall_pids"
	uninstall_validated_pids=""
	uninstall_identity_failed=0
	for uninstall_pid in $uninstall_pids; do
		uninstall_pid_state=0
		pid_is_exact_serve "$uninstall_pid" "$uninstall_stop_target" || uninstall_pid_state=$?
		case "$uninstall_pid_state" in
			0) uninstall_validated_pids="$uninstall_validated_pids $uninstall_pid" ;;
			2) uninstall_identity_failed=1 ;;
		esac
	done
	[ "$uninstall_identity_failed" -eq 0 ] || return 1
	for uninstall_pid in $uninstall_validated_pids; do
		kill -TERM "$uninstall_pid" 2>/dev/null || true
	done
	uninstall_tries=0
	while [ "$uninstall_tries" -lt 10 ]; do
		if ! uninstall_remaining="$(exact_serve_pids "$uninstall_stop_target")"; then
			echo "错误: 停止 serve 时无法重新读取进程表" >&2
			return 1
		fi
		[ -z "$uninstall_remaining" ] && return 0
		sleep 1
		uninstall_tries=$((uninstall_tries + 1))
	done
	echo "错误: serve 未在 TERM 后退出或被 KeepAlive 反复拉起: $uninstall_remaining" >&2
	echo "不会使用 KILL；请先禁用对应的 launchd/systemd 任务后重跑" >&2
	return 1
}

stop_manifest_serves() {
	uninstall_stop_failed=0
	while IFS="$(printf '\t')" read -r uninstall_kind uninstall_candidate; do
		[ -n "$uninstall_candidate" ] || continue
		if ! stop_exact_serves "$uninstall_candidate"; then
			uninstall_stop_failed=1
		fi
	done < "$uninstall_manifest"
	[ "$uninstall_stop_failed" -eq 0 ]
}

# 先固化受控部署清单：主路径、指向它的绝对/相对软链，以及与
# 它内容完全相同的已知部署副本。不会删除用户另行安装的同名程序。
printf 'target\t%s\n' "$installed_bin" > "$uninstall_manifest"
if [ "$installed_bin_logical" != "$installed_bin" ]; then
	printf 'process\t%s\n' "$installed_bin_logical" >> "$uninstall_manifest"
fi
for candidate in /usr/local/bin/iknowledge /usr/local/bin/iknowledge.exe \
	"$HOME/.local/bin/iknowledge" "$HOME/.local/bin/iknowledge.exe" \
	"$HOME/go/bin/iknowledge" "$HOME/go/bin/iknowledge.exe"; do
	[ "$candidate" != "$installed_bin" ] || continue
	candidate_parent="$(dirname "$candidate")"
	if [ -d "$candidate_parent" ]; then
		candidate_physical="$(cd -P "$candidate_parent" 2>/dev/null && pwd)/$(basename "$candidate")"
		if [ "$candidate_physical" = "$installed_bin" ]; then
			if [ "$candidate" != "$installed_bin_logical" ]; then
				printf 'process\t%s\n' "$candidate" >> "$uninstall_manifest"
			fi
			continue
		fi
	fi
	if symlink_points_to_installed_bin "$candidate"; then
		printf 'alias\t%s\n' "$candidate" >> "$uninstall_manifest"
	elif [ -f "$candidate" ] && [ -f "$installed_bin" ] && cmp -s "$candidate" "$installed_bin"; then
		printf 'alias\t%s\n' "$candidate" >> "$uninstall_manifest"
	fi
done

if [ "$platform" = "windows" ]; then
	echo "==> Windows 安全模式: 不按进程名终止 iknowledge.exe"
	echo "   若 $installed_bin 正在运行，请先只关闭该路径的 serve；脚本需在 Git Bash/MSYS2 中执行。"
else
	if ! uninstall_current_uid="$(id -u 2>/dev/null)"; then
		echo "错误: 无法确认当前 UID，卸载前尚未修改任何文件" >&2
		exit 1
	fi
	# 所有路径都安全停止后才开始删除，任一别名卡住都保留完整旧安装。
	if ! stop_manifest_serves; then
		echo "错误: 至少一个受控 serve 未停止；二进制、skill 和状态均未删除" >&2
		exit 1
	fi
fi

echo "==> 移除受控二进制与部署别名"
uninstall_remove_failed=0
while IFS="$(printf '\t')" read -r uninstall_kind uninstall_candidate; do
	[ -n "$uninstall_candidate" ] || continue
	[ "$uninstall_kind" != "process" ] || continue
	if ! rm -f "$uninstall_candidate"; then
		echo "错误: 无法删除 $uninstall_candidate" >&2
		uninstall_remove_failed=1
	fi
done < "$uninstall_manifest"
if [ "$uninstall_remove_failed" -ne 0 ]; then
	[ "$platform" != "windows" ] || echo "Windows 不允许删除运行中的 .exe；请只关闭该绝对路径后重跑。" >&2
	echo "skill 与私有状态未删除；处理上述精确路径后重跑即可收敛。" >&2
	exit 1
fi

if [ "$platform" != "windows" ]; then
	# 删除后连续两轮重扫，捕捉 launchd/systemd KeepAlive 的竞态拉起。
	if ! stop_manifest_serves; then
		echo "错误: 删除后仍有受控 serve；skill 与私有状态已保留" >&2
		exit 1
	fi
	sleep 1
	if ! stop_manifest_serves; then
		echo "错误: KeepAlive 在删除后重新拉起 serve；skill 与私有状态已保留" >&2
		exit 1
	fi
fi

echo "==> 移除 kb-bootstrap skill(Claude Code/Codex)"
CODEX_DIR="${CODEX_HOME:-$HOME/.codex}"
uninstall_skill_failed=0
if ! rm -rf "$HOME/.claude/skills/kb-bootstrap"; then
	echo "错误: 无法移除 Claude Code kb-bootstrap skill" >&2
	uninstall_skill_failed=1
fi
if ! rm -rf "$CODEX_DIR/skills/kb-bootstrap"; then
	echo "错误: 无法移除 Codex kb-bootstrap skill" >&2
	uninstall_skill_failed=1
fi
if [ "$uninstall_skill_failed" -ne 0 ]; then
	echo "私有状态已保留；修复权限后重跑即可收敛。" >&2
	exit 1
fi

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
