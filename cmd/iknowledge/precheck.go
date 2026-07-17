package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/store"
)

func runPrecheck(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("precheck", flag.ContinueOnError)
	repo := fs.String("repo", ".", "仓库路径")
	working := fs.Bool("working", false, "检查全部工作区改动(含暂存、未暂存、未跟踪);缺省只查暂存区")
	strict := fs.Bool("strict", false, "存在阻断项时返回非 0(适合团队门禁/CI)")
	jsonOut := fs.Bool("json", false, "输出结构化 JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s, err := store.Open(*repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	started := time.Now()
	files, err := gitPrecheckFiles(s.RepoRoot(), *working)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	deletedFiles, err := gitPrecheckDeletedFiles(s.RepoRoot(), *working)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	accountedNodes, err := gitPrecheckJournalNodes(s.RepoRoot(), *working, files)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	e := engine.New(s)
	report, err := e.Precheck(files, accountedNodes, deletedFiles)
	if err != nil {
		e.LogUsage(time.Now().UTC().Format("2006-01"), engine.UsageRecord{
			At: time.Now().UTC().Format(time.RFC3339), Tool: "cli_precheck", Source: "cli", OK: false,
			MS: time.Since(started).Milliseconds(),
		})
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	blocked := *strict && report.Blocking() > 0
	e.LogUsage(time.Now().UTC().Format("2006-01"), engine.UsageRecord{
		At: time.Now().UTC().Format(time.RFC3339), Tool: "cli_precheck", Source: "cli", OK: true,
		Warnings: len(report.Warnings), Blocked: blocked, MS: time.Since(started).Milliseconds(),
	})
	if *jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintln(os.Stderr, "错误:", err)
			return 1
		}
	} else {
		if _, err := fmt.Fprintln(out, report.Text()); err != nil {
			fmt.Fprintln(os.Stderr, "错误: 写 precheck 输出:", err)
			return 1
		}
	}
	if blocked {
		return 1
	}
	return 0
}

func gitPrecheckFiles(repo string, working bool) ([]string, error) {
	tracked, err := gitPrecheckTrackedFiles(repo, working, "ACMRDT")
	if err != nil {
		return nil, err
	}
	groups := [][]string{tracked}
	if working {
		untracked, err := gitNameOnly(repo, "ls-files", "--others", "--exclude-standard", "-z")
		if err != nil {
			return nil, fmt.Errorf("读取 git 未跟踪文件:%w", err)
		}
		groups = append(groups, untracked)
	}
	return mergeGitFileGroups(groups...), nil
}

func gitPrecheckDeletedFiles(repo string, working bool) ([]string, error) {
	return gitPrecheckTrackedFiles(repo, working, "D")
}

func gitPrecheckTrackedFiles(repo string, working bool, filter string) ([]string, error) {
	var groups [][]string
	if working {
		// HEAD 不存在(初始提交)时退化为 cached + worktree 两张 diff。
		if files, err := gitNameOnly(repo, "diff", "HEAD", "--name-only", "--diff-filter="+filter, "-z"); err == nil {
			groups = append(groups, files)
		} else {
			cached, cachedErr := gitNameOnly(repo, "diff", "--cached", "--name-only", "--diff-filter="+filter, "-z")
			unstaged, unstagedErr := gitNameOnly(repo, "diff", "--name-only", "--diff-filter="+filter, "-z")
			if cachedErr != nil || unstagedErr != nil {
				return nil, fmt.Errorf("读取 git 工作区:%v / %v", cachedErr, unstagedErr)
			}
			groups = append(groups, cached, unstaged)
		}
	} else {
		cached, err := gitNameOnly(repo, "diff", "--cached", "--name-only", "--diff-filter="+filter, "-z")
		if err != nil {
			return nil, fmt.Errorf("读取 git 暂存区:%w", err)
		}
		groups = append(groups, cached)
	}
	return mergeGitFileGroups(groups...), nil
}

func mergeGitFileGroups(groups ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, group := range groups {
		for _, file := range group {
			file = filepath.ToSlash(file)
			if file != "" && !seen[file] {
				seen[file] = true
				out = append(out, file)
			}
		}
	}
	sort.Strings(out)
	return out
}

func gitNameOnly(repo string, args ...string) ([]string, error) {
	data, err := gitOutput(repo, args...)
	if err != nil {
		return nil, err
	}
	var files []string
	for part := range bytes.SplitSeq(data, []byte{0}) {
		if len(part) > 0 {
			files = append(files, string(part))
		}
	}
	return files, nil
}

func gitOutput(repo string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gitArgs := append([]string{"-C", repo}, args...)
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	data, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("git 命令超时")
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// gitPrecheckJournalNodes 只提取相对 HEAD 新出现的 journal 记录。目标内容在
// 缺省模式从 index 读取,在 --working 模式从工作树读取;同 ID 的既有/被改写行
// 不算新记账,从而维持 journal 追加式语义。
func gitPrecheckJournalNodes(repo string, working bool, changedFiles []string) ([]string, error) {
	seenNode := map[string]bool{}
	var nodes []string
	for _, file := range changedFiles {
		rel := filepath.ToSlash(file)
		if !strings.HasPrefix(rel, ".knowledge/journal/") || !strings.HasSuffix(rel, ".jsonl") {
			continue
		}
		base, _, err := gitObject(repo, "HEAD:"+rel)
		if err != nil {
			return nil, fmt.Errorf("读取 journal 基线 %s:%w", rel, err)
		}
		var target []byte
		if working {
			target, err = os.ReadFile(filepath.Join(repo, filepath.FromSlash(rel)))
			if os.IsNotExist(err) {
				continue
			}
		} else {
			var ok bool
			target, ok, err = gitObject(repo, ":"+rel)
			if err == nil && !ok {
				continue // journal 删除不构成本次新增记账
			}
		}
		if err != nil {
			return nil, fmt.Errorf("读取 journal 目标 %s:%w", rel, err)
		}

		baseIDs := map[string]bool{}
		for _, rec := range parsePrecheckJournal(base) {
			if rec.ID != "" {
				baseIDs[rec.ID] = true
			}
		}
		for _, rec := range parsePrecheckJournal(target) {
			if !rec.valid() || baseIDs[rec.ID] {
				continue
			}
			for _, node := range rec.Nodes {
				node = strings.TrimSpace(node)
				if node != "" && !seenNode[node] {
					seenNode[node] = true
					nodes = append(nodes, node)
				}
			}
		}
	}
	sort.Strings(nodes)
	return nodes, nil
}

type precheckJournalRecord struct {
	ID    string    `json:"id"`
	Nodes []string  `json:"nodes"`
	At    time.Time `json:"at"`
	What  string    `json:"what"`
	Why   string    `json:"why"`
}

func (r precheckJournalRecord) valid() bool {
	return validPrecheckChangeID(r.ID) && !r.At.IsZero() && len(r.Nodes) > 0 &&
		strings.TrimSpace(r.What) != "" && strings.TrimSpace(r.Why) != ""
}

func validPrecheckChangeID(id string) bool {
	// chg_ + 20060102T150405Z + _ + 16 lowercase hex (model.NewChangeID)。
	if len(id) != 37 || !strings.HasPrefix(id, "chg_") || id[20] != '_' {
		return false
	}
	if _, err := time.Parse("20060102T150405Z", id[4:20]); err != nil {
		return false
	}
	random, err := hex.DecodeString(id[21:])
	return err == nil && len(random) == 8 && id[21:] == strings.ToLower(id[21:])
}

func parsePrecheckJournal(data []byte) []precheckJournalRecord {
	var out []precheckJournalRecord
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rec precheckJournalRecord
		if json.Unmarshal(line, &rec) == nil {
			out = append(out, rec)
		}
	}
	return out
}

// gitObject 读取一个 Git blob。对象或 HEAD 尚不存在时返回 ok=false;仓库、权限、
// 超时等真正的执行错误仍上抛。调用前已有 git diff 成功,exit 128 在这里即缺对象。
func gitObject(repo, spec string) ([]byte, bool, error) {
	data, err := gitOutput(repo, "show", spec)
	if err == nil {
		return data, true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 128 {
		return nil, false, nil
	}
	return nil, false, err
}
