package engine

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// TruthGitHealth reports whether the durable knowledge truth is actually
// protected by the repository Git index. Merely having .knowledge/.gitignore
// and .gitattributes is insufficient: an outer .gitignore may discard the
// whole directory, and an umbrella workspace may not be a Git repository at
// all.
type TruthGitHealth struct {
	State   string // tracked | partial | untracked | ignored | no-git | unavailable
	Tracked int
	Files   int
	Detail  string
}

func (h TruthGitHealth) protected() bool {
	return h.State == "tracked" && h.Files > 0 && h.Tracked == h.Files
}

func (e *Engine) truthGitHealthCachedContext(ctx context.Context) TruthGitHealth {
	e.rt.truthGitMu.Lock()
	if e.rt.truthGit.State != "" && e.now().Sub(e.rt.truthGitAt) < time.Minute {
		health := e.rt.truthGit
		e.rt.truthGitMu.Unlock()
		return health
	}
	e.rt.truthGitMu.Unlock()

	health := inspectTruthGit(ctx, e.Store.RepoRoot(), e.Store.Dir())
	// A canceled request must not poison the next healthy status call for a full
	// TTL. Other unavailable states (no git binary, permission error) are stable
	// enough to cache and still remain advisory rather than blocking the tool.
	if ctx.Err() != nil {
		return health
	}
	e.rt.truthGitMu.Lock()
	e.rt.truthGit, e.rt.truthGitAt = health, e.now()
	e.rt.truthGitMu.Unlock()
	return health
}

func inspectTruthGit(ctx context.Context, repo, knowledgeDir string) TruthGitHealth {
	if _, err := exec.LookPath("git"); err != nil {
		return TruthGitHealth{State: "unavailable", Detail: "PATH 中找不到 git"}
	}
	topOut, err := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return TruthGitHealth{State: "no-git", Detail: "知识库根不在 Git 工作树内"}
	}
	top := strings.TrimSpace(string(topOut))
	if physical, evalErr := filepath.EvalSymlinks(top); evalErr == nil {
		top = physical
	}
	files, err := durableKnowledgeFiles(knowledgeDir)
	if err != nil {
		return TruthGitHealth{State: "unavailable", Detail: "枚举知识正本失败: " + err.Error()}
	}
	health := TruthGitHealth{State: "untracked", Files: len(files)}
	if len(files) == 0 {
		health.Detail = "没有可跟踪的知识正本文件"
		return health
	}

	paths := make([]string, 0, len(files))
	for _, file := range files {
		if physical, evalErr := filepath.EvalSymlinks(file); evalErr == nil {
			file = physical
		}
		rel, relErr := filepath.Rel(top, file)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return TruthGitHealth{State: "unavailable", Files: len(files), Detail: "知识正本不在 Git 根目录内"}
		}
		paths = append(paths, filepath.ToSlash(rel))
	}

	// --no-index also exposes an outer ignore rule when a previously force-added
	// path happens to remain tracked. That is still unhealthy for new shards.
	var ignoredIn bytes.Buffer
	for _, p := range paths {
		ignoredIn.WriteString(p)
		ignoredIn.WriteByte(0)
	}
	ignoreCmd := exec.CommandContext(ctx, "git", "-C", top, "check-ignore", "--no-index", "-z", "--stdin")
	ignoreCmd.Stdin = &ignoredIn
	ignoredOut, ignoreErr := ignoreCmd.Output()
	if ignoreErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(ignoreErr, &exitErr) || exitErr.ExitCode() != 1 {
			return TruthGitHealth{State: "unavailable", Files: len(files), Detail: "检查 Git ignore 失败: " + ignoreErr.Error()}
		}
	}
	ignored := countNULRecords(ignoredOut)

	physicalKnowledgeDir := knowledgeDir
	if physical, evalErr := filepath.EvalSymlinks(knowledgeDir); evalErr == nil {
		physicalKnowledgeDir = physical
	}
	knowledgeRel, err := filepath.Rel(top, physicalKnowledgeDir)
	if err != nil {
		return TruthGitHealth{State: "unavailable", Files: len(files), Detail: "计算知识目录路径失败: " + err.Error()}
	}
	trackedOut, err := exec.CommandContext(ctx, "git", "-C", top, "ls-files", "-z", "--", filepath.ToSlash(knowledgeRel)).Output()
	if err != nil {
		return TruthGitHealth{State: "unavailable", Files: len(files), Detail: "读取 Git 索引失败: " + err.Error()}
	}
	trackedSet := make(map[string]struct{})
	for _, item := range bytes.Split(trackedOut, []byte{0}) {
		if len(item) > 0 {
			trackedSet[string(item)] = struct{}{}
		}
	}
	for _, p := range paths {
		if _, ok := trackedSet[p]; ok {
			health.Tracked++
		}
	}

	switch {
	case ignored == len(paths):
		health.State = "ignored"
		health.Detail = "知识正本被 Git ignore 规则整体排除"
	case ignored > 0:
		health.State = "partial"
		health.Detail = "部分知识正本被 Git ignore 规则排除"
	case health.Tracked == len(paths):
		health.State = "tracked"
		health.Detail = "全部知识正本已进入 Git 索引"
	case health.Tracked > 0:
		health.State = "partial"
		health.Detail = "只有部分知识正本进入 Git 索引"
	default:
		health.State = "untracked"
		health.Detail = "知识正本尚未进入 Git 索引"
	}
	return health
}

func durableKnowledgeFiles(knowledgeDir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(knowledgeDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(knowledgeDir, path)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		first, _, _ := strings.Cut(rel, "/")
		if entry.IsDir() {
			if first == "local" || first == "wip" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(entry.Name(), ".tmp") {
			return nil
		}
		allowed := rel == "project.yaml" || rel == "config.yaml" || rel == ".gitattributes" || rel == ".gitignore" ||
			first == "tree" || first == "journal" || first == "flows" || first == "topics"
		if allowed {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

func countNULRecords(data []byte) int {
	count := bytes.Count(data, []byte{0})
	if len(data) > 0 && data[len(data)-1] != 0 {
		count++
	}
	return count
}
