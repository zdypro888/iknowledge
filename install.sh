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
SKILL_REF=""
SKILL_LOCAL_FILE=""
BINARY_SOURCE=""
BINARY_EXPECTED_VERSION=""
BINARY_COMMITTED=0
BINARY_FATAL=0
SOURCE_DOWNGRADE_EXPLICIT=0

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

bin_name="iknowledge"
[ "$os" = "windows" ] && bin_name="iknowledge.exe"

# ---- 选择安装目录 ----
install_dir_requested="${IKNOWLEDGE_BIN:-$HOME/.local/bin}"
mkdir -p "$install_dir_requested"
install_dir_logical="$(cd "$install_dir_requested" && pwd -L)"
install_dir="$(cd -P "$install_dir_requested" && pwd)"
target_bin="$install_dir/$bin_name"
target_bin_logical="$install_dir_logical/$bin_name"
PATH_BOOTSTRAP_LINK=0
PATH_BOOTSTRAP_CREATED=0

is_known_deployment_path() {
	candidate_path="$1"
	case "$candidate_path" in
		/usr/local/bin/iknowledge|/usr/local/bin/iknowledge.exe|\
		"$HOME/.local/bin/iknowledge"|"$HOME/.local/bin/iknowledge.exe"|\
		"$HOME/go/bin/iknowledge"|"$HOME/go/bin/iknowledge.exe") return 0 ;;
	esac
	return 1
}

symlink_points_to_target() {
	link_path="$1"
	want_target="$2"
	[ -L "$link_path" ] || return 1
	link_value="$(readlink "$link_path" 2>/dev/null)" || return 1
	case "$link_value" in
		/*) link_absolute="$link_value" ;;
		*) link_absolute="$(cd -P "$(dirname "$link_path")" 2>/dev/null && pwd)/$link_value" ;;
	esac
	link_parent="$(dirname "$link_absolute")"
	[ -d "$link_parent" ] || return 1
	link_absolute="$(cd -P "$link_parent" 2>/dev/null && pwd)/$(basename "$link_absolute")"
	[ "$link_absolute" = "$want_target" ]
}

path_contains_dir() {
	want_dir="$1"
	if [ -d "$want_dir" ]; then
		want_dir="$(cd -P "$want_dir" 2>/dev/null && pwd)"
	fi
	old_ifs="$IFS"
	IFS=:
	for path_dir in ${PATH:-}; do
		[ -n "$path_dir" ] || path_dir="."
		if [ -d "$path_dir" ]; then
			path_absolute="$(cd -P "$path_dir" 2>/dev/null && pwd || true)"
			if [ "$path_absolute" = "$want_dir" ]; then
				IFS="$old_ifs"
				return 0
			fi
		fi
	done
	IFS="$old_ifs"
	return 1
}

target_will_resolve_first() {
	path_scan_old_ifs="$IFS"
	IFS=:
	for path_scan_dir in ${PATH:-}; do
		[ -n "$path_scan_dir" ] || path_scan_dir="."
		[ -d "$path_scan_dir" ] || continue
		path_scan_absolute="$(cd -P "$path_scan_dir" 2>/dev/null && pwd || true)"
		if [ "$path_scan_absolute" = "$install_dir" ]; then
			IFS="$path_scan_old_ifs"
			return 0
		fi
		# 模拟新 target 创建后的 PATH 解析：若更早的目录已有可执行
		# iknowledge，本次安装不会成为默认命令，必须 fail closed。
		if [ -x "$path_scan_absolute/$bin_name" ] && [ ! -d "$path_scan_absolute/$bin_name" ]; then
			IFS="$path_scan_old_ifs"
			return 1
		fi
	done
	IFS="$path_scan_old_ifs"
	return 1
}

preflight_path_resolution() {
	preflight_resolved="$(command -v iknowledge 2>/dev/null || true)"
	# target 尚不存在时 command -v 会跳过 install_dir。若它在 PATH 中
	# 位于当前命令之前，新文件提交后会正确覆盖解析，不属于 shadow。
	if target_will_resolve_first; then
		return 0
	fi
	if [ -n "$preflight_resolved" ]; then
		if [ "$preflight_resolved" = "$target_bin" ]; then
			return 0
		fi
		if is_known_deployment_path "$preflight_resolved"; then
			if symlink_points_to_target "$preflight_resolved" "$target_bin"; then
				return 0
			fi
			if { [ -e "$target_bin" ] || [ -L "$target_bin" ]; } && [ -f "$preflight_resolved" ] && cmp -s "$preflight_resolved" "$target_bin"; then
				return 0
			fi
		fi
		echo "错误: PATH 中的 iknowledge ($preflight_resolved) 不属于本次受控部署；已安装文件尚未替换" >&2
		return 1
	fi
	if path_contains_dir "$install_dir"; then
		return 0
	fi
	if [ "$os" != "windows" ] && path_contains_dir /usr/local/bin; then
		bootstrap_path="/usr/local/bin/iknowledge"
		if [ -e "$bootstrap_path" ] || [ -L "$bootstrap_path" ]; then
			if symlink_points_to_target "$bootstrap_path" "$target_bin"; then
				return 0
			fi
			if { [ -e "$target_bin" ] || [ -L "$target_bin" ]; } && [ -f "$bootstrap_path" ] && cmp -s "$bootstrap_path" "$target_bin"; then
				return 0
			fi
			echo "错误: $bootstrap_path 已存在且不属于本次受控部署；拒绝覆盖，已安装文件尚未替换" >&2
			return 1
		fi
		if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
			PATH_BOOTSTRAP_LINK=1
			return 0
		fi
	fi
	echo "错误: 安装前检查确认 $install_dir 不在 PATH，且没有可安全创建的 /usr/local/bin 入口；已安装文件尚未替换" >&2
	echo "请先把 $install_dir 加入 PATH 后重跑安装器" >&2
	return 1
}

prepare_path_bootstrap() {
	[ "$PATH_BOOTSTRAP_LINK" -eq 1 ] || return 0
	bootstrap_path="/usr/local/bin/iknowledge"
	if [ -e "$bootstrap_path" ] || [ -L "$bootstrap_path" ]; then
		echo "错误: PATH 预检后 $bootstrap_path 被其他进程创建，拒绝覆盖" >&2
		return 1
	fi
	if ! ln -s "$target_bin" "$bootstrap_path"; then
		echo "错误: 无法预置 ${bootstrap_path}；已安装版本尚未替换" >&2
		return 1
	fi
	PATH_BOOTSTRAP_CREATED=1
	return 0
}

rollback_path_bootstrap() {
	[ "$PATH_BOOTSTRAP_CREATED" -eq 1 ] || return 0
	if symlink_points_to_target /usr/local/bin/iknowledge "$target_bin"; then
		rm -f /usr/local/bin/iknowledge
	fi
	PATH_BOOTSTRAP_CREATED=0
}

cleanup_install_exit() {
	if [ "$BINARY_COMMITTED" -eq 0 ]; then
		rollback_path_bootstrap
	fi
	if [ -n "${tmpdir:-}" ] && [ -d "$tmpdir" ]; then
		rm -rf "$tmpdir"
	fi
	if [ -n "${build_dir:-}" ] && [ -d "$build_dir" ]; then
		rm -rf "$build_dir"
	fi
}
trap cleanup_install_exit EXIT

# ---- 安全换代：argv 只做候选筛选，内核报告的 executable path 才是身份 ----
# 不用 pkill -f / taskkill /IM：它们会误杀别处安装、别的用户或同名程序。
resolve_controlled_executable() {
	resolve_path="$1"
	resolve_hops=0
	while [ -L "$resolve_path" ]; do
		[ "$resolve_hops" -lt 16 ] || return 1
		resolve_link="$(readlink "$resolve_path" 2>/dev/null)" || return 1
		resolve_parent="$(cd -P "$(dirname "$resolve_path")" 2>/dev/null && pwd)" || return 1
		case "$resolve_link" in
			/*) resolve_path="$resolve_link" ;;
			*) resolve_path="$resolve_parent/$resolve_link" ;;
		esac
		resolve_hops=$((resolve_hops + 1))
	done
	resolve_parent="$(cd -P "$(dirname "$resolve_path")" 2>/dev/null && pwd)" || return 1
	printf '%s/%s\n' "$resolve_parent" "$(basename "$resolve_path")"
}

pid_executable_path() {
	executable_pid="$1"
	executable_target="$2"
	case "$os" in
		linux)
			readlink "/proc/$executable_pid/exe" 2>/dev/null
			;;
		darwin)
			if [ -x /usr/sbin/lsof ]; then
				lsof_command=/usr/sbin/lsof
			elif command -v lsof >/dev/null 2>&1; then
				lsof_command="$(command -v lsof)"
			else
				return 1
			fi
			lsof_output="$("$lsof_command" -a -p "$executable_pid" -d txt -Fn 2>/dev/null)" || return 1
			executable_path=""
			executable_target_path="$(resolve_controlled_executable "$executable_target")" || return 1
			while IFS= read -r lsof_line; do
				case "$lsof_line" in
					n/*)
						lsof_path="${lsof_line#n}"
						case "$lsof_path" in *" (deleted)") lsof_path="${lsof_path% (deleted)}" ;; esac
						[ -n "$executable_path" ] || executable_path="$lsof_path"
						lsof_resolved="$(resolve_controlled_executable "$lsof_path" 2>/dev/null || true)"
						if [ "$lsof_resolved" = "$executable_target_path" ]; then
							printf '%s\n' "$lsof_path"
							return 0
						fi
						;;
				esac
			done <<EOF
$lsof_output
EOF
			[ -n "$executable_path" ] || return 1
			printf '%s\n' "$executable_path"
			;;
		*) return 1 ;;
	esac
}

serve_command_candidate() {
	candidate_command="$1"
	candidate_target="$2"
	case "$candidate_command" in
		"$candidate_target serve"|"$candidate_target serve "*|\
		"iknowledge serve"|"iknowledge serve "*|\
		"iknowledge.exe serve"|"iknowledge.exe serve "*) return 0 ;;
	esac
	return 1
}

# 返回 0=身份确认属于 target；1=进程消失/其他安装；2=候选仍在但无法证明身份。
pid_is_exact_serve() {
	pid="$1"
	target="$2"
	if ! current_uid="$(id -u 2>/dev/null)" || ! process_line="$(ps -p "$pid" -o uid=,args= 2>/dev/null)"; then
		return 1
	fi
	owner_uid=""
	command=""
	read -r owner_uid command <<EOF
$process_line
EOF
	[ "$owner_uid" = "$current_uid" ] || return 1
	serve_command_candidate "$command" "$target" || return 1
	if ! executable_path="$(pid_executable_path "$pid" "$target")"; then
		echo "错误: 发现当前 UID 的 iknowledge serve 候选 PID ${pid}，但无法证明其 executable identity；拒绝继续，请手动确认并停止后重跑" >&2
		return 2
	fi
	case "$executable_path" in
		*" (deleted)") executable_path="${executable_path% (deleted)}" ;;
	esac
	case "$executable_path" in
		/*) ;;
		*)
			echo "错误: PID $pid 的 executable identity 不是绝对路径；拒绝继续" >&2
			return 2
			;;
	esac
	if ! executable_path="$(resolve_controlled_executable "$executable_path")" || ! controlled_path="$(resolve_controlled_executable "$target")"; then
		echo "错误: 无法规范化 PID $pid 或受控安装路径的 executable identity；拒绝继续" >&2
		return 2
	fi
	[ "$executable_path" = "$controlled_path" ] || return 1
	return 0
}

exact_serve_pids() {
	target="$1"
	if ! current_uid="$(id -u 2>/dev/null)" || ! process_list="$(ps -axo uid=,pid=,args= 2>/dev/null)"; then
		return 1
	fi
	scan_pids=""
	scan_failed=0
	while read -r owner_uid pid command; do
		[ "$owner_uid" = "$current_uid" ] || continue
		serve_command_candidate "$command" "$target" || continue
		pid_state=0
		pid_is_exact_serve "$pid" "$target" || pid_state=$?
		case "$pid_state" in
			0) scan_pids="$scan_pids $pid" ;;
			2) scan_failed=1 ;;
		esac
	done <<EOF
$process_list
EOF
	[ "$scan_failed" -eq 0 ] || return 1
	printf '%s\n' "$scan_pids" | sed 's/^[[:space:]]*//'
}

stop_installed_serve() {
	target="$1"
	[ "$os" != "windows" ] || return 0
	if ! command -v ps >/dev/null 2>&1; then
		echo "错误: 无法检查旧 serve；请手动停止由 $target 启动的服务后重跑" >&2
		return 1
	fi
	if ! pids="$(exact_serve_pids "$target")"; then
		echo "错误: 无法读取进程表以检查由 $target 启动的旧 serve" >&2
		return 1
	fi
	[ -n "$pids" ] || return 0
	echo "==> 发现由当前安装路径启动的旧 serve，发送 TERM: $pids"
	validated_pids=""
	identity_failed=0
	for pid in $pids; do
		pid_state=0
		pid_is_exact_serve "$pid" "$target" || pid_state=$?
		case "$pid_state" in
			0) validated_pids="$validated_pids $pid" ;;
			2) identity_failed=1 ;;
		esac
	done
	[ "$identity_failed" -eq 0 ] || return 1
	for pid in $validated_pids; do
		kill -TERM "$pid" 2>/dev/null || true
	done
	remaining="$pids"
	tries=0
	while [ -n "$remaining" ] && [ "$tries" -lt 10 ]; do
		remaining=""
		identity_failed=0
		for pid in $pids; do
			pid_state=0
			pid_is_exact_serve "$pid" "$target" || pid_state=$?
			case "$pid_state" in
				0) remaining="$remaining $pid" ;;
				2) identity_failed=1 ;;
			esac
		done
		if [ "$identity_failed" -ne 0 ]; then
			echo "错误: TERM 后无法继续证明旧 serve 的 executable identity；拒绝继续替换" >&2
			return 1
		fi
		[ -z "$remaining" ] && break
		sleep 1
		tries=$((tries + 1))
	done
	if [ -n "$remaining" ]; then
		echo "错误: 旧 serve 未在 TERM 后退出:$remaining" >&2
		echo "请先安全结束这些由 $target 启动的进程，再重跑安装器；安装器不会使用 KILL。" >&2
		return 1
	fi
	echo "   旧 serve 已优雅退出；下次 stdio 连接会自动拉起新版本"
	return 0
}

# 仅把已知部署位置中“指向当前 target 的 symlink”或“与当前 target 内容完全相同
# 的普通文件”视为受控别名。先于替换记录，避免 target 换代后失去旧内容关系。
stage_deployment_aliases() {
	source_bin="$1"
	target="$2"
	manifest="$3"
	if ! : > "$manifest"; then
		echo "错误: 无法初始化部署别名清单；尚未替换主二进制" >&2
		return 1
	fi
	# argv 保留启动者使用的逻辑路径。macOS 的 /var→/private/var 或
	# IKNOWLEDGE_BIN 父目录软链会让它与物理 target 不同；只用于停服，不参与文件替换。
	if [ "$target_bin_logical" != "$target" ]; then
		if ! printf 'process\t%s\t-\n' "$target_bin_logical" >> "$manifest"; then
			echo "错误: 无法记录逻辑部署路径；尚未替换主二进制" >&2
			return 1
		fi
	fi
	[ -e "$target" ] || [ -L "$target" ] || return 0
	for candidate in /usr/local/bin/iknowledge /usr/local/bin/iknowledge.exe \
		"$HOME/.local/bin/iknowledge" "$HOME/.local/bin/iknowledge.exe" \
		"$HOME/go/bin/iknowledge" "$HOME/go/bin/iknowledge.exe"; do
		[ "$candidate" != "$target" ] || continue
		candidate_parent="$(dirname "$candidate")"
		if [ -d "$candidate_parent" ]; then
			candidate_physical="$(cd -P "$candidate_parent" 2>/dev/null && pwd)/$(basename "$candidate")"
			if [ "$candidate_physical" = "$target" ]; then
				if [ "$candidate" != "$target_bin_logical" ]; then
					printf 'process\t%s\t-\n' "$candidate" >> "$manifest" || return 1
				fi
				continue
			fi
		fi
		if [ -L "$candidate" ]; then
			symlink_points_to_target "$candidate" "$target" || continue
			if ! printf 'link\t%s\t-\n' "$candidate" >> "$manifest"; then
				echo "错误: 无法记录部署别名 ${candidate}；尚未替换主二进制" >&2
				return 1
			fi
			continue
		fi
		[ -f "$candidate" ] && cmp -s "$candidate" "$target" || continue
		alias_dir="$(dirname "$candidate")"
		alias_stage="$(mktemp "$alias_dir/.iknowledge-alias.XXXXXX")" || {
			echo "错误: 无法暂存部署别名 ${candidate}；尚未替换主二进制" >&2
			return 1
		}
		if ! cp "$source_bin" "$alias_stage" || ! chmod +x "$alias_stage"; then
			rm -f "$alias_stage"
			echo "错误: 无法暂存部署别名 ${candidate}；尚未替换主二进制" >&2
			return 1
		fi
		if ! printf 'copy\t%s\t%s\n' "$candidate" "$alias_stage" >> "$manifest"; then
			rm -f "$alias_stage"
			echo "错误: 无法记录部署别名 ${candidate}；尚未替换主二进制" >&2
			return 1
		fi
	done
	return 0
}

stop_manifest_serves() {
	manifest="$1"
	manifest_stop_failed=0
	while IFS="$(printf '\t')" read -r kind candidate manifest_stage; do
		[ -n "$candidate" ] || continue
		if ! stop_installed_serve "$candidate"; then
			manifest_stop_failed=1
		fi
	done < "$manifest"
	[ "$manifest_stop_failed" -eq 0 ]
}

stop_deployment_serves() {
	manifest="$1"
	deployment_stop_failed=0
	if ! stop_installed_serve "$target_bin"; then
		deployment_stop_failed=1
	fi
	if ! stop_manifest_serves "$manifest"; then
		deployment_stop_failed=1
	fi
	[ "$deployment_stop_failed" -eq 0 ]
}

cleanup_alias_stages() {
	manifest="$1"
	[ -f "$manifest" ] || return 0
	while IFS="$(printf '\t')" read -r kind candidate manifest_stage; do
		if [ "$kind" = "copy" ] && [ -n "$manifest_stage" ]; then
			rm -f "$manifest_stage"
		fi
	done < "$manifest"
}

install_staged_binary() {
	source_bin="$1"
	BINARY_COMMITTED=0
	stage="$(mktemp "$install_dir/.iknowledge-install.XXXXXX")" || {
		echo "   无法在安装目录创建临时文件" >&2
		return 1
	}
	if ! cp "$source_bin" "$stage" || ! chmod +x "$stage"; then
		rm -f "$stage"
		echo "   设置执行权限失败" >&2
		return 1
	fi
	alias_manifest="$(mktemp "$install_dir/.iknowledge-aliases.XXXXXX")" || {
		rm -f "$stage"
		echo "   无法创建部署别名清单" >&2
		return 1
	}
	if ! stage_deployment_aliases "$source_bin" "$target_bin" "$alias_manifest"; then
		cleanup_alias_stages "$alias_manifest"
		rm -f "$alias_manifest" "$stage"
		return 1
	fi
	if ! stop_deployment_serves "$alias_manifest"; then
		cleanup_alias_stages "$alias_manifest"
		rm -f "$alias_manifest" "$stage"
		return 1
	fi
	if ! mv -f "$stage" "$target_bin"; then
		cleanup_alias_stages "$alias_manifest"
		rm -f "$alias_manifest" "$stage"
		if [ "$os" = "windows" ]; then
			echo "错误: Windows 不允许覆盖仍在运行的 .exe。请只结束由 $target_bin 启动的 serve 后，在 Git Bash/MSYS2 中重跑。" >&2
			echo "安装器不会用 taskkill /IM 按名称广杀同名进程。" >&2
		else
			echo "   原子安装二进制失败" >&2
		fi
		return 1
	fi
	BINARY_COMMITTED=1
	# 同步此前确认属于同一部署的普通文件别名；symlink 会自动看到新 target。
	while IFS="$(printf '\t')" read -r kind candidate alias_stage; do
		[ "$kind" = "copy" ] || continue
		if ! mv -f "$alias_stage" "$candidate"; then
			failed_alias="$candidate"
			cleanup_alias_stages "$alias_manifest"
			rm -f "$alias_manifest"
			echo "错误: 主二进制已更新，但受控别名 $failed_alias 原子换代失败；请勿启动服务，先重跑安装器" >&2
			return 1
		fi
	done < "$alias_manifest"
	# launchd KeepAlive/systemd Restart 可能在第一次 TERM 与 rename 之间拉起旧
	# inode。rename 后再精确扫两次；被终止者此后只能从新文件重启。
	post_stop_failed=0
	if ! stop_deployment_serves "$alias_manifest"; then
		post_stop_failed=1
	fi
	sleep 1
	if ! stop_deployment_serves "$alias_manifest"; then
		post_stop_failed=1
	fi
	cleanup_alias_stages "$alias_manifest"
	rm -f "$alias_manifest"
	if [ "$post_stop_failed" -ne 0 ]; then
		echo "错误: 二进制已更新，但替换后仍有受控 serve 无法优雅停止；安装器不会谎报成功" >&2
		return 1
	fi
	return 0
}

is_formal_semver() {
	semver_value="$1"
	case "$semver_value" in
		v[0-9]*.[0-9]*.[0-9]*) ;;
		*) return 1 ;;
	esac
	semver_old_ifs="$IFS"
	IFS=.
	set -- ${semver_value#v}
	IFS="$semver_old_ifs"
	[ "$#" -eq 3 ] || return 1
	for semver_part in "$@"; do
		case "$semver_part" in
			""|*[!0-9]*) return 1 ;;
		esac
		case "$semver_part" in
			0|[1-9]|[1-9][0-9]*) ;;
			*) return 1 ;;
		esac
	done
	return 0
}

decimal_component_gt() {
	decimal_left="$1"
	decimal_right="$2"
	decimal_left_len=${#decimal_left}
	decimal_right_len=${#decimal_right}
	[ "$decimal_left_len" -gt "$decimal_right_len" ] && return 0
	[ "$decimal_left_len" -lt "$decimal_right_len" ] && return 1
	[ "$decimal_left" != "$decimal_right" ] || return 1
	decimal_last="$(printf '%s\n%s\n' "$decimal_left" "$decimal_right" | LC_ALL=C sort | tail -n 1)"
	[ "$decimal_last" = "$decimal_left" ]
}

semver_gt() {
	semver_left="${1#v}"
	semver_right="${2#v}"
	semver_old_ifs="$IFS"
	IFS=.; set -- $semver_left; IFS="$semver_old_ifs"
	semver_left_major="$1"; semver_left_minor="$2"; semver_left_patch="$3"
	IFS=.; set -- $semver_right; IFS="$semver_old_ifs"
	semver_right_major="$1"; semver_right_minor="$2"; semver_right_patch="$3"
	for semver_pair in \
		"$semver_left_major:$semver_right_major" \
		"$semver_left_minor:$semver_right_minor" \
		"$semver_left_patch:$semver_right_patch"; do
		semver_pair_left=${semver_pair%%:*}
		semver_pair_right=${semver_pair#*:}
		decimal_component_gt "$semver_pair_left" "$semver_pair_right" && return 0
		decimal_component_gt "$semver_pair_right" "$semver_pair_left" && return 1
	done
	return 1
}

# ---- 下载预编译二进制 ----
install_binary() {
    # 查最新 Release tag(GitHub API;无 tag 时返回空 → 回退源码构建)。
    latest_tag="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
        | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)"
	if ! is_formal_semver "$latest_tag"; then
		[ -z "$latest_tag" ] || echo "   releases/latest tag 不是正式 vMAJOR.MINOR.PATCH，拒绝作为预编译来源: $latest_tag" >&2
        return 1
	fi
	if [ -x "$target_bin" ] && [ ! -d "$target_bin" ]; then
		installed_version_output="$("$target_bin" version 2>/dev/null || true)"
		installed_version="${installed_version_output#iknowledge }"
		installed_version="${installed_version%% *}"
		if is_formal_semver "$installed_version" && semver_gt "$installed_version" "$latest_tag"; then
			echo "错误: 已安装正式版本 $installed_version 严格高于 releases/latest ${latest_tag}；拒绝自动降级" >&2
			echo "如确需降级，请显式设置 IKNOWLEDGE_FORCE_SOURCE=1 和不可变 IKNOWLEDGE_SOURCE_REF。" >&2
			BINARY_FATAL=1
			return 1
		fi
	fi
    asset="iknowledge-${os}-${arch}.tar.gz"
    url="https://github.com/${REPO}/releases/download/${latest_tag}/${asset}"
	if ! tmpdir="$(mktemp -d)" || [ -z "$tmpdir" ]; then
		echo "   无法创建临时目录" >&2
		return 1
	fi

    echo "==> 下载预编译二进制 ${latest_tag}/${asset}"
	if ! curl -fsSL "$url" -o "$tmpdir/$asset"; then
        echo "   下载失败(可能该平台尚无预编译包)" >&2
        return 1
    fi
	# 在替换旧 binary 前先把同 tag 的 skill 一并取到私有临时目录。这样网络失败不会留下
	# “新 binary + 旧/main skill”的半升级状态。skill 是同一 Release 的受校验资产，
	# 不从可另行移动的 raw tag 路径现取。
	skill_asset="kb-bootstrap-SKILL.md"
	release_skill_url="https://github.com/${REPO}/releases/download/${latest_tag}/${skill_asset}"
	if ! curl -fsSL "$release_skill_url" -o "$tmpdir/kb-bootstrap-SKILL.md" || [ ! -s "$tmpdir/kb-bootstrap-SKILL.md" ]; then
		echo "   同 Release 的 kb-bootstrap skill 资产不可用，拒绝安装" >&2
		return 1
	fi

	# 校验 sha256：缺 checksums、缺本资产记录、机器无校验工具均 fail closed。
	sums_url="https://github.com/${REPO}/releases/download/${latest_tag}/sha256sums.txt"
	if ! curl -fsSL "$sums_url" -o "$tmpdir/sha256sums.txt" 2>/dev/null; then
		echo "   缺 sha256sums.txt，拒绝安装未校验 Release 资产" >&2
		return 1
	fi
	awk -v a="$asset" '$2 == a || $2 == "*" a { print }' "$tmpdir/sha256sums.txt" > "$tmpdir/$asset.sha256"
	awk -v a="$skill_asset" '$2 == a || $2 == "*" a { print }' "$tmpdir/sha256sums.txt" > "$tmpdir/$skill_asset.sha256"
	if [ ! -s "$tmpdir/$asset.sha256" ]; then
		echo "   checksums 中缺 ${asset}，拒绝安装" >&2
		return 1
	fi
	if [ ! -s "$tmpdir/$skill_asset.sha256" ]; then
		echo "   checksums 中缺 ${skill_asset}，拒绝安装" >&2
		return 1
	fi
	if command -v sha256sum >/dev/null 2>&1; then
		(cd "$tmpdir" && sha256sum -c "$asset.sha256" "$skill_asset.sha256" >/dev/null 2>&1) || {
			echo "   sha256 校验失败" >&2
			return 1
		}
	elif command -v shasum >/dev/null 2>&1; then
		(cd "$tmpdir" && shasum -a 256 -c "$asset.sha256" "$skill_asset.sha256" >/dev/null 2>&1) || {
			echo "   sha256 校验失败" >&2
			return 1
		}
	else
		echo "   找不到 sha256sum/shasum，拒绝安装未校验 Release 资产" >&2
		return 1
	fi
	echo "   sha256 校验通过"

    # 解压。先要求归档恰好只有预期的根文件，拒绝 ../、绝对路径、额外链接等。
    inner="iknowledge-${os}-${arch}"
    [ "$os" = "windows" ] && inner="$inner.exe"
	members="$(tar tzf "$tmpdir/$asset" 2>/dev/null || true)"
	if [ "$members" != "$inner" ]; then
		echo "   归档成员异常，拒绝解压: $members" >&2
		return 1
	fi
	if ! tar xzf "$tmpdir/$asset" -C "$tmpdir" "$inner"; then
		echo "   解压失败" >&2
		return 1
	fi
    if [ ! -f "$tmpdir/$inner" ]; then
        echo "   解压后未找到 $inner" >&2
        return 1
    fi
	# 保留已校验产物；skill 目标全部成功 staging 后才替换 binary。
	BINARY_SOURCE="$tmpdir/$inner"
	BINARY_EXPECTED_VERSION="$latest_tag"
	# release 二进制与 skill 必须来自同一不可变 tag，不能混用 main。
	SKILL_REF="$latest_tag"
	SKILL_LOCAL_FILE="$tmpdir/kb-bootstrap-SKILL.md"
    echo "   已校验并暂存 → $BINARY_SOURCE"
}

# ---- Go 源码构建(兜底) ----
install_from_source() {
    echo "==> 从源码构建(需要 Go 工具链)"
	SKILL_LOCAL_FILE=""
	BINARY_EXPECTED_VERSION=""
    if ! command -v go >/dev/null 2>&1; then
        echo "错误: 需要 Go 环境(https://go.dev/dl/),或等 Release 发布预编译包后重跑" >&2
        exit 1
    fi
	requested_ref="${IKNOWLEDGE_SOURCE_REF:-latest}"
	case "$requested_ref" in
		""|*[!A-Za-z0-9._/-]*|*..*)
			echo "错误: IKNOWLEDGE_SOURCE_REF 只能是安全的 tag/branch/commit ref" >&2
			exit 1
			;;
	esac
	source_dir=""
	source_skill=""
	build_ref=""
	if [ "$requested_ref" = "local" ]; then
		# 本地源码模式必须真由 install.sh 文件启动；curl | sh 的 $0=sh 不得把 cwd 当源码。
		case "$0" in
			install.sh|*/install.sh)
				if [ -f "$0" ]; then
					source_dir="$(cd "$(dirname "$0")" 2>/dev/null && pwd || true)"
				fi
				;;
		esac
		if [ -z "$source_dir" ] || [ ! -f "$source_dir/go.mod" ] || [ ! -f "$source_dir/skills/kb-bootstrap/SKILL.md" ]; then
			echo "错误: IKNOWLEDGE_SOURCE_REF=local 只允许从完整源码树里的 install.sh 启动" >&2
			exit 1
		fi
		build_ref="local"
		source_skill="$source_dir/skills/kb-bootstrap/SKILL.md"
		SKILL_REF="local-explicit"
	else
		if ! resolved_ref="$(go list -m -f '{{.Version}}' "github.com/${REPO}@${requested_ref}" 2>/dev/null)"; then
			echo "错误: 无法把源码 ref $requested_ref 解析为不可变 module version；拒绝使用 latest/main/branch 移动窗口" >&2
			exit 1
		fi
		case "$resolved_ref" in
			""|*[!A-Za-z0-9.+-]*|*..*)
				echo "错误: Go 返回了不安全的源码版本: $resolved_ref" >&2
				exit 1
				;;
		esac
		resolved_base="${resolved_ref%%-*}"
		resolved_base="${resolved_base%%+*}"
		if ! is_formal_semver "$resolved_base"; then
			echo "错误: Go 返回的源码版本不是不可变 semver/pseudo-version: $resolved_ref" >&2
			exit 1
		fi
		if ! go mod download "github.com/${REPO}@${resolved_ref}" >/dev/null 2>&1; then
			echo "错误: 无法下载已冻结源码版本 $resolved_ref" >&2
			exit 1
		fi
		if ! downloaded_ref="$(go list -m -f '{{.Version}}' "github.com/${REPO}@${resolved_ref}" 2>/dev/null)" || [ "$downloaded_ref" != "$resolved_ref" ]; then
			echo "错误: 下载后的 module version 与冻结 ref 不一致；拒绝构建" >&2
			exit 1
		fi
		if ! source_dir="$(go list -m -f '{{.Dir}}' "github.com/${REPO}@${resolved_ref}" 2>/dev/null)" || [ -z "$source_dir" ]; then
			echo "错误: 无法定位已冻结源码快照 $resolved_ref" >&2
			exit 1
		fi
		source_skill="$source_dir/skills/kb-bootstrap/SKILL.md"
		if [ ! -f "$source_dir/go.mod" ] || [ ! -s "$source_skill" ]; then
			echo "错误: 已冻结 module 快照缺少 go.mod 或非空 kb-bootstrap skill；拒绝构建" >&2
			exit 1
		fi
		build_ref="$resolved_ref"
		SKILL_REF="$resolved_ref"
		BINARY_EXPECTED_VERSION="$resolved_ref"
		if [ "$SOURCE_DOWNGRADE_EXPLICIT" -eq 0 ] && is_formal_semver "$resolved_ref" && [ -x "$target_bin" ] && [ ! -d "$target_bin" ]; then
			source_installed_output="$("$target_bin" version 2>/dev/null || true)"
			source_installed_version="${source_installed_output#iknowledge }"
			source_installed_version="${source_installed_version%% *}"
			if is_formal_semver "$source_installed_version" && semver_gt "$source_installed_version" "$resolved_ref"; then
				echo "错误: 已安装正式版本 $source_installed_version 严格高于默认源码回退 ${resolved_ref}；拒绝自动降级" >&2
				echo "如确需降级，请同时显式设置 IKNOWLEDGE_FORCE_SOURCE=1 与 IKNOWLEDGE_SOURCE_REF=${resolved_ref}。" >&2
				exit 1
			fi
		fi
	fi
	if ! build_dir="$(mktemp -d)" || [ -z "$build_dir" ]; then
		echo "错误: 无法创建源码构建临时目录" >&2
		exit 1
	fi
	# skill 只从与 binary 相同的源码快照读取一次，再复制给所有宿主。
	SKILL_LOCAL_FILE="$build_dir/kb-bootstrap-SKILL.md"
	if ! cp "$source_skill" "$SKILL_LOCAL_FILE" || [ ! -s "$SKILL_LOCAL_FILE" ]; then
		echo "错误: 无法冻结同版本 kb-bootstrap skill" >&2
		exit 1
	fi
	if [ "$build_ref" = "local" ]; then
		if ! (cd "$source_dir" && GOBIN="$build_dir" go install ./cmd/iknowledge); then
			echo "错误: 本地源码构建失败" >&2
			exit 1
		fi
	elif ! GOBIN="$build_dir" go install "github.com/${REPO}/cmd/iknowledge@${build_ref}"; then
		echo "错误: 源码构建失败" >&2
		exit 1
	fi
	BINARY_SOURCE="$build_dir/$bin_name"
	echo "   已构建并暂存 → $BINARY_SOURCE"
}

# ---- 主安装逻辑 ----
BIN=""
if [ -n "${IKNOWLEDGE_FORCE_SOURCE:-}" ]; then
	if [ -n "${IKNOWLEDGE_SOURCE_REF:-}" ]; then
		SOURCE_DOWNGRADE_EXPLICIT=1
	fi
	install_from_source
	BIN="$target_bin"
elif install_binary; then
	BIN="$target_bin"
else
	if [ "$BINARY_FATAL" -ne 0 ]; then
		exit 1
	fi
	echo "==> 预编译包不可用,回退源码构建"
	# install_binary 可能已创建 release 临时目录后才发现资产不可用；回退前主动回收，
	# 避免随后 source trap 覆盖 EXIT trap 留下垃圾。
	if [ -n "${tmpdir:-}" ] && [ -d "$tmpdir" ]; then
		rm -rf "$tmpdir"
	fi
	install_from_source
	BIN="$target_bin"
fi

if ! preflight_path_resolution; then
	exit 1
fi

# ---- skill 暂存：权限/磁盘/网络失败都发生在主二进制替换之前 ----
stage_skill() {
	dst="$1"
	mkdir -p "$dst"
	if [ -d "$dst/SKILL.md" ]; then
		echo "错误: skill 目标 $dst/SKILL.md 是目录，拒绝在替换 binary 后才失败" >&2
		return 1
	fi
	stage_skill="$(mktemp "$dst/.SKILL.md.XXXXXX")" || {
		echo "错误: 无法创建 skill 临时文件" >&2
		return 1
	}
	# 该变量只能由脚本内部设置：release 是替换 binary 前预取的同-tag 文件，local 是经过
	# $0/install.sh + go.mod 双重确认的源码树；curl | sh 的恶意 cwd 永远进不了这里。
	if [ -n "$SKILL_LOCAL_FILE" ] && [ -f "$SKILL_LOCAL_FILE" ]; then
        if ! cp "$SKILL_LOCAL_FILE" "$stage_skill"; then
			rm -f "$stage_skill"
			return 1
		fi
    else
		SKILL_URL="https://raw.githubusercontent.com/${REPO}/${SKILL_REF}/skills/kb-bootstrap/SKILL.md"
		if ! curl -fsSL "$SKILL_URL" -o "$stage_skill"; then
			rm -f "$stage_skill"
			return 1
		fi
    fi
	if [ ! -s "$stage_skill" ]; then
		rm -f "$stage_skill"
		echo "错误: kb-bootstrap skill 为空，拒绝提交" >&2
		return 1
	fi
	printf '%s\n' "$stage_skill"
}

echo "==> 暂存 kb-bootstrap skill(Claude Code)"
echo "   来源 ref: $SKILL_REF"
CLAUDE_SKILL_DST="$HOME/.claude/skills/kb-bootstrap"
if ! CLAUDE_SKILL_STAGE="$(stage_skill "$CLAUDE_SKILL_DST")"; then
	echo "错误: Claude Code skill 暂存失败；主二进制尚未替换" >&2
	exit 1
fi

CODEX_DIR="${CODEX_HOME:-$HOME/.codex}"
CODEX_SKILL_DST=""
CODEX_SKILL_STAGE=""
if [ -d "$CODEX_DIR" ]; then
    echo "==> 检测到 Codex,暂存同 ref skill"
	CODEX_SKILL_DST="$CODEX_DIR/skills/kb-bootstrap"
	if ! CODEX_SKILL_STAGE="$(stage_skill "$CODEX_SKILL_DST")"; then
		rm -f "$CLAUDE_SKILL_STAGE"
		echo "错误: Codex skill 暂存失败；主二进制尚未替换" >&2
		exit 1
	fi
else
    echo "==> 未检测到 Codex($CODEX_DIR 不存在),跳过"
fi

if [ -z "$BINARY_SOURCE" ] || [ ! -x "$BINARY_SOURCE" ]; then
	rm -f "$CLAUDE_SKILL_STAGE" "$CODEX_SKILL_STAGE"
	echo "错误: 安装暂存产物不可执行:$BINARY_SOURCE" >&2
	exit 1
fi
# 在替换任何已安装文件前先执行暂存 binary 并验证版本。
if ! version_output="$("$BINARY_SOURCE" version)"; then
	rm -f "$CLAUDE_SKILL_STAGE" "$CODEX_SKILL_STAGE"
	echo "错误: 暂存二进制 version 自检失败；已安装版本未替换" >&2
	exit 1
fi
case "$version_output" in
	"iknowledge "[![:space:]]*) ;;
	*)
		rm -f "$CLAUDE_SKILL_STAGE" "$CODEX_SKILL_STAGE"
		echo "错误: 暂存二进制 version 输出格式非法: $version_output" >&2
		exit 1
		;;
esac
if [ -n "$BINARY_EXPECTED_VERSION" ]; then
	case "$version_output" in
		"iknowledge $BINARY_EXPECTED_VERSION"|"iknowledge $BINARY_EXPECTED_VERSION "*) ;;
		*)
			rm -f "$CLAUDE_SKILL_STAGE" "$CODEX_SKILL_STAGE"
			echo "错误: 暂存二进制自报版本与来源 ref 不符: expected=$BINARY_EXPECTED_VERSION got=$version_output" >&2
			exit 1
			;;
	esac
fi
if ! prepare_path_bootstrap; then
	rm -f "$CLAUDE_SKILL_STAGE" "$CODEX_SKILL_STAGE"
	exit 1
fi
binary_commit_issue=0
if ! install_staged_binary "$BINARY_SOURCE"; then
	if [ "$BINARY_COMMITTED" -eq 0 ]; then
		rollback_path_bootstrap
		rm -f "$CLAUDE_SKILL_STAGE" "$CODEX_SKILL_STAGE"
		echo "错误: 二进制换代失败；skill 尚未提交" >&2
		exit 1
	fi
	# target 已换代后才发现别名或 KeepAlive 问题时，仍提交同 ref
	# skills，避免人为制造“新 binary + 旧 skill”；末尾保持非零退出且不宣称完成。
	binary_commit_issue=1
	echo "警告: 主二进制已换代，将先提交同 ref skill 以保持版本一致，但本次安装最终仍会报失败" >&2
fi
if ! mv -f "$CLAUDE_SKILL_STAGE" "$CLAUDE_SKILL_DST/SKILL.md"; then
	rm -f "$CLAUDE_SKILL_STAGE" "$CODEX_SKILL_STAGE"
	echo "错误: 二进制已更新，但 Claude Code skill 原子提交失败；请立即重跑安装器" >&2
	exit 1
fi
if [ -n "$CODEX_SKILL_STAGE" ] && ! mv -f "$CODEX_SKILL_STAGE" "$CODEX_SKILL_DST/SKILL.md"; then
	rm -f "$CODEX_SKILL_STAGE"
	echo "错误: 二进制已更新，但 Codex skill 原子提交失败；请立即重跑安装器" >&2
	exit 1
fi

# 暂存 binary 已在提交前完成执行/版本自检，此处只回显那个已验证结果。
printf '%s\n' "$version_output"
echo "   二进制与已检测宿主 skill 已提交 → $BIN"

# ---- PATH 可解析性与旧版 shadow 检查(stdio/hook 需要裸命令) ----
# stdio 桥由 MCP 客户端直接 spawn(GUI 启动的客户端 PATH 常没有 ~/.local/bin),
# 尽力软链进 /usr/local/bin 保证裸命令可解析;不行则明确提示。
# 提交前 command -v 可能让某些 POSIX shell 记住旧 shadow；此处必须
# 重新按新 target 已存在的 PATH 状态解析。
hash -r 2>/dev/null || true
resolved_bin="$(command -v iknowledge 2>/dev/null || true)"
if [ -z "$resolved_bin" ]; then
	echo "错误: 新二进制已安装到 ${BIN}，但 iknowledge 仍不在 PATH；stdio/hook 无法可靠启动。" >&2
	if [ "$os" = "windows" ]; then
		echo "请把 $install_dir 加入当前用户 PATH 后重跑安装器。" >&2
	else
		echo "请把 $install_dir 加入 PATH，或执行: sudo ln -sf \"$BIN\" /usr/local/bin/iknowledge" >&2
	fi
	exit 1
fi
# 不能只看 command -v 成功：旧 /usr/local/bin 可能 shadow 新 ~/.local/bin，
# 让用户以为安全更新已生效、实际 MCP 仍跑旧漏洞版本。cmp 同时兼容真实文件与 symlink。
if ! cmp -s "$resolved_bin" "$BIN"; then
	echo "错误: PATH 中的 iknowledge ($resolved_bin) 不是刚安装的 ${BIN}。" >&2
	echo "请移除/更新旧 shadow，确保 command -v iknowledge 指向新二进制后重跑。" >&2
	exit 1
fi
echo "==> PATH 校验通过: $resolved_bin"

if [ "$binary_commit_issue" -ne 0 ]; then
	echo "错误: binary/skill 已保持同 ref，但受控别名或旧 serve 仍需人工处理；解决诊断后重跑安装器" >&2
	exit 1
fi

echo ""
echo "完成。现在进入任意项目,对 Claude Code 或 Codex 说:"
echo "  「初始化当前项目知识库」"
