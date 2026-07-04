package engine

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/zdypro888/iknowledge/internal/parser"
	"github.com/zdypro888/iknowledge/internal/store"
)

// listSourceFiles 枚举待索引源文件(impl §6 第 1 步定案):
// git 仓库用 `git ls-files -co --exclude-standard`(含未跟踪的新文件、排除 ignored——
// 纯 ls-files 列不出用户新建还没 add 的文件,骨架会缺节点);非 git 仓库回退 WalkDir。
// 之后按注册扩展名 + 默认排除段 + config include/exclude 过滤;生成代码在读内容后另筛。
// 返回正斜杠相对路径,字典序。
func listSourceFiles(repo string, reg *parser.Registry, cfg *store.Config) ([]string, error) {
	rels, gitOK := gitListFiles(repo)
	if !gitOK {
		var err error
		rels, err = walkListFiles(repo)
		if err != nil {
			return nil, err
		}
	}

	var out []string
	for _, rel := range rels {
		if rel == "" || reg.ForFile(rel) == nil || parser.ExcludedPath(rel) {
			continue
		}
		if !cfgAllows(cfg, rel) {
			continue
		}
		// ls-files 会列出 index 里有、工作区已删的文件——以工作区为准。
		if st, err := os.Stat(filepath.Join(repo, filepath.FromSlash(rel))); err != nil || st.IsDir() {
			continue
		}
		out = append(out, rel)
	}
	sort.Strings(out)
	return out, nil
}

// gitChangeCounts 统计近 since 内每文件的提交触碰次数(热区排序的频率因子,
// knowledge.md §12.1)。git 不可用/非仓库返回 nil——热度退化为纯中心度。
// rename 只计新路径(近似:旧路径热度沉底,新路径从头计,可接受)。
func gitChangeCounts(repo, since string) map[string]int {
	cmd := exec.Command("git", "-C", repo, "log", "--since="+since, "--name-only", "--pretty=format:")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	counts := map[string]int{}
	for line := range strings.SplitSeq(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			counts[filepath.ToSlash(line)]++
		}
	}
	return counts
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
	cmd := exec.Command("git", "-C", repo, "ls-files", "-co", "--exclude-standard", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	var rels []string
	for b := range bytes.SplitSeq(out, []byte{0}) {
		if len(b) > 0 {
			rels = append(rels, filepath.ToSlash(string(b)))
		}
	}
	return rels, true
}

func walkListFiles(repo string) ([]string, error) {
	var rels []string
	err := filepath.WalkDir(repo, func(p string, d os.DirEntry, err error) error {
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
