package engine

import (
	"bytes"
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
