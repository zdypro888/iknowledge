package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zdypro888/iknowledge/internal/model"
	"github.com/zdypro888/iknowledge/internal/parser"
	"github.com/zdypro888/iknowledge/internal/store"
)

// listSourceFiles 枚举待索引源文件(impl §6 第 1 步定案):
// git 仓库用 `git ls-files -co --exclude-standard`(含未跟踪的新文件、排除 ignored——
// 纯 ls-files 列不出用户新建还没 add 的文件,骨架会缺节点);非 git 仓库回退 WalkDir。
// 之后按注册扩展名 + 默认排除段 + config include/exclude 过滤;生成代码在读内容后另筛。
// 返回正斜杠相对路径,字典序。
func listSourceFiles(repo string, reg *parser.Registry, cfg *store.Config) ([]string, error) {
	return listSourceFilesContext(context.Background(), repo, reg, cfg)
}

func listSourceFilesContext(ctx context.Context, repo string, reg *parser.Registry, cfg *store.Config) ([]string, error) {
	if ctx == nil {
		return nil, fmt.Errorf("list source files: nil context")
	}
	rels, gitOK, err := gitListFilesContext(ctx, repo)
	if err != nil {
		return nil, err
	}
	if !gitOK {
		rels, err = walkListFilesContext(ctx, repo)
		if err != nil {
			return nil, err
		}
	}

	var out []string
	for i, rel := range rels {
		if err := contextCheckpoint(ctx, i); err != nil {
			return nil, err
		}
		if rel == "" || reg.ForFile(rel) == nil || parser.ExcludedPath(rel) {
			continue
		}
		if !cfgAllows(cfg, rel) {
			continue
		}
		// ls-files 会列出 index 里有、工作区已删的文件——以工作区为准。
		if st, err := safeRepoFileInfo(repo, rel); err != nil || !st.Mode().IsRegular() {
			continue
		}
		out = append(out, rel)
	}
	if err := contextSortStrings(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// safeRepoPathInfo 把仓库根本身解析为物理路径（允许用户从 symlink 根打开），
// 但拒绝根以下任一 symlink/非目录中间组件。源码路径来自可提交的 git index，
// 不能让恶意仓库用 tracked symlink 读取仓外文件。
func safeRepoPathInfo(repo, rel string) (string, os.FileInfo, error) {
	clean, ok := model.SafeRel(filepath.ToSlash(rel))
	if !ok {
		return "", nil, fmt.Errorf("非法仓库相对路径 %q", rel)
	}
	root, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return "", nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", nil, err
	}
	cur := root
	parts := strings.Split(clean, "/")
	var info os.FileInfo
	for i, part := range parts {
		cur = filepath.Join(cur, filepath.FromSlash(part))
		info, err = os.Lstat(cur)
		if err != nil {
			return "", nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", nil, fmt.Errorf("仓库路径包含 symlink:%s", clean)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return "", nil, fmt.Errorf("仓库路径中间组件不是目录:%s", clean)
		}
	}
	return cur, info, nil
}

func safeRepoFileInfo(repo, rel string) (os.FileInfo, error) {
	_, info, err := safeRepoPathInfo(repo, rel)
	return info, err
}

func safeRepoRead(repo, rel string) ([]byte, error) {
	path, before, err := safeRepoPathInfo(repo, rel)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("仓库路径不是普通文件:%s", rel)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, statErr := f.Stat()
	if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = f.Close()
		if statErr != nil {
			return nil, statErr
		}
		return nil, fmt.Errorf("仓库文件在打开期间被替换:%s", rel)
	}
	data, readErr := io.ReadAll(f)
	closeErr := f.Close()
	_, after, afterErr := safeRepoPathInfo(repo, rel)
	if afterErr == nil && !os.SameFile(opened, after) {
		afterErr = fmt.Errorf("仓库文件在读取期间被替换:%s", rel)
	}
	return data, errors.Join(readErr, closeErr, afterErr)
}

// gitChangeCounts 统计近 since 内每文件的提交触碰次数(热区排序的频率因子,
// knowledge.md §12.1)。git 不可用/非仓库返回 nil——热度退化为纯中心度。
// rename 只计新路径(近似:旧路径热度沉底,新路径从头计,可接受)。
func gitChangeCounts(repo, since string) map[string]int {
	counts, _ := gitChangeCountsContext(context.Background(), repo, since)
	return counts
}

func gitChangeCountsContext(ctx context.Context, repo, since string) (map[string]int, error) {
	if ctx == nil {
		return nil, fmt.Errorf("git change counts: nil context")
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "log", "--since="+since, "--name-only", "--pretty=format:")
	out, err := cmd.Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, nil
	}
	counts := map[string]int{}
	i := 0
	for line := range strings.SplitSeq(string(out), "\n") {
		if err := contextCheckpoint(ctx, i); err != nil {
			return nil, err
		}
		i++
		if line = strings.TrimSpace(line); line != "" {
			counts[filepath.ToSlash(line)]++
		}
	}
	return counts, nil
}

// gitTrail 取文件的近期提交线索(knowledge.md §15 三期"git 历史挖掘初始来时路"的
// 机械落地,2026-07-04):不做全量挖掘,只在侦查简报附上目标区域"为什么长这样"的
// 档案入口——深挖(git show/blame)由侦察兵按需自取。非 git 仓库返回空。
func gitTrail(repo string, files []string) string {
	var b strings.Builder
	for _, f := range files {
		// --follow:文件改名后来时路不断链(单文件 follow 成本毫秒级)。
		out, err := exec.Command("git", "-C", repo, "log", "-n", "3", "--follow",
			"--date=short", "--pretty=format:%h %ad %s", "--", f).Output()
		trimmed := strings.TrimSpace(string(out))
		if err != nil || trimmed == "" {
			continue
		}
		fmt.Fprintf(&b, "  %s:\n", f)
		for line := range strings.SplitSeq(trimmed, "\n") {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	return b.String()
}

// gitListFiles 用 -z 输出防路径含特殊字符;git 不可用/非仓库返回 ok=false。
func gitListFiles(repo string) ([]string, bool) {
	rels, ok, _ := gitListFilesContext(context.Background(), repo)
	return rels, ok
}

func gitListFilesContext(ctx context.Context, repo string) ([]string, bool, error) {
	if ctx == nil {
		return nil, false, fmt.Errorf("git list files: nil context")
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "ls-files", "-co", "--exclude-standard", "-z")
	out, err := cmd.Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, false, ctxErr
	}
	if err != nil {
		return nil, false, nil
	}
	var rels []string
	i := 0
	for b := range bytes.SplitSeq(out, []byte{0}) {
		if err := contextCheckpoint(ctx, i); err != nil {
			return nil, false, err
		}
		i++
		if len(b) > 0 {
			rels = append(rels, filepath.ToSlash(string(b)))
		}
	}
	return rels, true, nil
}

func walkListFiles(repo string) ([]string, error) {
	return walkListFilesContext(context.Background(), repo)
}

func walkListFilesContext(ctx context.Context, repo string) ([]string, error) {
	var rels []string
	i := 0
	err := filepath.WalkDir(repo, func(p string, d os.DirEntry, err error) error {
		if err := contextCheckpoint(ctx, i); err != nil {
			return err
		}
		i++
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(repo, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			// 隐藏目录(.git、.knowledge 等)整树跳过;根目录自身除外。
			if rel != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		rels = append(rels, rel)
		return nil
	})
	return rels, err
}

// cfgAllows 应用 config.yaml 的 include/exclude 覆盖(impl §5):
// include 非空时仅索引匹配项;exclude 追加排除。模式用 path.Match 语法匹配
// 正斜杠相对路径,"dir/" 结尾的模式按前缀匹配整棵子树。
func cfgAllows(cfg *store.Config, rel string) bool {
	if cfg == nil {
		return true
	}
	match := func(pat string) bool {
		if strings.HasSuffix(pat, "/") {
			return strings.HasPrefix(rel, pat)
		}
		ok, err := path.Match(pat, rel)
		return err == nil && ok
	}
	if slices.ContainsFunc(cfg.Exclude, match) {
		return false
	}
	return len(cfg.Include) == 0 || slices.ContainsFunc(cfg.Include, match)
}
