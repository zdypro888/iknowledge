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

bin_name="iknowledge"
[ "$os" = "windows" ] && bin_name="iknowledge.exe"

# ---- 选择安装目录 ----
install_dir="${IKNOWLEDGE_BIN:-$HOME/.local/bin}"
mkdir -p "$install_dir"
install_dir="$(cd "$install_dir" && pwd)"

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
	if ! tmpdir="$(mktemp -d)" || [ -z "$tmpdir" ]; then
		echo "   无法创建临时目录" >&2
		return 1
	fi
    trap 'rm -rf "$tmpdir"' EXIT

    echo "==> 下载预编译二进制 ${latest_tag}/${asset}"
    if ! curl -fsSL "$url" -o "$tmpdir/$asset"; then
        echo "   下载失败(可能该平台尚无预编译包)" >&2
        return 1
    fi

	# 校验 sha256：缺 checksums、缺本资产记录、机器无校验工具均 fail closed。
	sums_url="https://github.com/${REPO}/releases/download/${latest_tag}/sha256sums.txt"
	if ! curl -fsSL "$sums_url" -o "$tmpdir/sha256sums.txt" 2>/dev/null; then
		echo "   缺 sha256sums.txt，拒绝安装未校验二进制" >&2
		return 1
	fi
	awk -v a="$asset" '$2 == a || $2 == "*" a { print }' "$tmpdir/sha256sums.txt" > "$tmpdir/$asset.sha256"
	if [ ! -s "$tmpdir/$asset.sha256" ]; then
		echo "   checksums 中缺 $asset，拒绝安装" >&2
		return 1
	fi
	if command -v sha256sum >/dev/null 2>&1; then
		(cd "$tmpdir" && sha256sum -c "$asset.sha256" >/dev/null 2>&1) || {
			echo "   sha256 校验失败" >&2
			return 1
		}
	elif command -v shasum >/dev/null 2>&1; then
		(cd "$tmpdir" && shasum -a 256 -c "$asset.sha256" >/dev/null 2>&1) || {
			echo "   sha256 校验失败" >&2
			return 1
		}
	else
		echo "   找不到 sha256sum/shasum，拒绝安装未校验二进制" >&2
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
	# 在目标目录内 staging，再 rename 覆盖；下载/复制/chmod 任一步失败都保留旧版本。
	stage="$(mktemp "$install_dir/.iknowledge-install.XXXXXX")" || {
		echo "   无法在安装目录创建临时文件" >&2
		return 1
	}
	if ! cp "$tmpdir/$inner" "$stage" || ! chmod +x "$stage"; then
		rm -f "$stage"
		echo "   设置执行权限失败" >&2
		return 1
	fi
	if ! mv -f "$stage" "$install_dir/$bin_name"; then
		rm -f "$stage"
		echo "   原子安装二进制失败" >&2
		return 1
	fi
    echo "   已安装 → $install_dir/$bin_name"
}

# ---- Go 源码构建(兜底) ----
install_from_source() {
    echo "==> 从源码构建(需要 Go 工具链)"
    if ! command -v go >/dev/null 2>&1; then
        echo "错误: 需要 Go 环境(https://go.dev/dl/),或等 Release 发布预编译包后重跑" >&2
        exit 1
    fi
	if ! GOBIN="$install_dir" go install "github.com/${REPO}/cmd/iknowledge@latest"; then
		echo "错误: 源码构建失败" >&2
		exit 1
	fi
	echo "   已安装 → $install_dir/$bin_name"
}

# ---- 主安装逻辑 ----
BIN=""
if [ -n "${IKNOWLEDGE_FORCE_SOURCE:-}" ]; then
	install_from_source
	BIN="$install_dir/$bin_name"
elif install_binary; then
	BIN="$install_dir/$bin_name"
else
	echo "==> 预编译包不可用,回退源码构建"
	install_from_source
	BIN="$install_dir/$bin_name"
fi

# version 自检。
"$BIN" version

# ---- skill 安装 ----
fetch_skill() {
	dst="$1"
	mkdir -p "$dst"
	stage_skill="$(mktemp "$dst/.SKILL.md.XXXXXX")" || {
		echo "错误: 无法创建 skill 临时文件" >&2
		return 1
	}
	SELF_DIR=""
	# curl | sh 时 $0 通常是 sh，绝不能把当前工作目录误认成安装器目录。
	# 只有确实从名为 install.sh 的本地文件启动时才允许复用同仓 skill。
	case "$0" in
		install.sh|*/install.sh)
			if [ -f "$0" ]; then
				SELF_DIR="$(cd "$(dirname "$0")" 2>/dev/null && pwd || true)"
			fi
			;;
	esac
	if [ -n "$SELF_DIR" ] && [ -f "$SELF_DIR/skills/kb-bootstrap/SKILL.md" ]; then
        if ! cp "$SELF_DIR/skills/kb-bootstrap/SKILL.md" "$stage_skill"; then
			rm -f "$stage_skill"
			return 1
		fi
    else
		if ! curl -fsSL "$SKILL_URL" -o "$stage_skill"; then
			rm -f "$stage_skill"
			return 1
		fi
    fi
	if ! mv -f "$stage_skill" "$dst/SKILL.md"; then
		rm -f "$stage_skill"
		return 1
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

# ---- PATH 可解析性与旧版 shadow 检查(stdio/hook 需要裸命令) ----
# stdio 桥由 MCP 客户端直接 spawn(GUI 启动的客户端 PATH 常没有 ~/.local/bin),
# 尽力软链进 /usr/local/bin 保证裸命令可解析;不行则明确提示。
resolved_bin="$(command -v iknowledge 2>/dev/null || true)"
if [ -z "$resolved_bin" ]; then
    if [ -w /usr/local/bin ] || ln -sf "$BIN" /usr/local/bin/iknowledge 2>/dev/null; then
        ln -sf "$BIN" /usr/local/bin/iknowledge 2>/dev/null || true
    fi
	resolved_bin="$(command -v iknowledge 2>/dev/null || true)"
fi
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

echo ""
echo "完成。现在进入任意项目,对 Claude Code 或 Codex 说:"
echo "  「初始化当前项目知识库」"
