package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// R29 批次4 知识导出/导入(impl §留痕:纯 stdlib archive/tar+compress/gzip,零新依赖)。
//
// 用途:repo 间知识迁移(把参考仓的 .knowledge/ 种到新仓)+ 纯文本备份。
// Node-ID 是 repo-relative 的,跨仓迁移需路径前缀重映射(如 internal/auth/ → pkg/auth/)。
//
// 格式:tar.gz,保留 .knowledge/ 相对结构(tree/ journal/ flows/ topics/ config.yaml),
// 排除 local/(token/usage 等本机态)与 wip/(任务态)。bundle 头部一条 MANIFEST.json。

// Export 把 .knowledge/ 的 tree+journal+flows+topics+config 打包成 tar.gz 写入 w。
// 前提:已 EnsureRuntime(不用持锁——只读文件系统)。
func (e *Engine) Export(w io.Writer) error {
	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	dir := e.Store.Dir()
	// MANIFEST:导出元数据(时间、schema、来源仓)。
	manifest := fmt.Sprintf(`{"exported_at":"%s","repo":"%s","schema":1}`,
		e.now().UTC().Format(time.RFC3339), filepath.ToSlash(e.Store.RepoRoot()))
	if err := writeTarFile(tw, "MANIFEST.json", []byte(manifest)); err != nil {
		return err
	}

	// 收集 tree/ journal/ flows/ topics/ config.yaml(排除 local/ wip/)。
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "local" || name == "wip" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		// 只含知识制品(yaml/jsonl),不打包临时/锁文件。
		if !strings.HasSuffix(relSlash, ".yaml") && !strings.HasSuffix(relSlash, ".jsonl") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return writeTarFile(tw, relSlash, data)
	})
}

const maxImportEntryBytes = 16 << 20

// ImportOptions 控制 .kbundle 导入。DryRun 只解析报告不写盘;Backup 在写入前把
// 当前知识库导出到 .knowledge/local/import-backups/(仍遵守工具只写 .knowledge)。
type ImportOptions struct {
	PathRemap     map[string]string
	DryRun        bool
	Backup        bool
	MaxEntryBytes int64
}

// ImportReport 是导入预演/执行报告。
type ImportReport struct {
	Scanned    int
	Imported   int
	Skipped    int
	Bytes      int64
	BackupPath string
	Entries    []ImportEntry
}

type ImportEntry struct {
	Name   string
	Action string // import | skip
	Reason string
	Bytes  int64
}

func (r ImportReport) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "导入报告: scanned=%d importable=%d skipped=%d bytes=%d", r.Scanned, r.Imported, r.Skipped, r.Bytes)
	if r.BackupPath != "" {
		fmt.Fprintf(&b, "\n备份: %s", r.BackupPath)
	}
	shown := r.Entries
	if len(shown) > 20 {
		shown = shown[:20]
	}
	for _, ent := range shown {
		if ent.Action == "import" {
			fmt.Fprintf(&b, "\n  + %s (%d bytes)", ent.Name, ent.Bytes)
		} else {
			fmt.Fprintf(&b, "\n  - %s (%s)", ent.Name, ent.Reason)
		}
	}
	if len(r.Entries) > len(shown) {
		fmt.Fprintf(&b, "\n  ... 另有 %d 条", len(r.Entries)-len(shown))
	}
	return b.String()
}

// Import 从 r 读 tar.gz bundle,解包到目标 .knowledge/ 的 tree+journal+flows+topics。
// pathRemap 可选:把 Node-ID 的路径前缀 from 映射到 to(跨仓迁移,如 "internal/auth/" → "pkg/auth/")。
// 已存在的文件会被覆盖(导入是合并语义:同名 shard 后者覆盖前者)。
// 不碰源码(铁律二),只写目标 .knowledge/。
func (e *Engine) Import(r io.Reader, pathRemap map[string]string) (int, error) {
	rep, err := e.ImportWithOptions(r, ImportOptions{PathRemap: pathRemap})
	return rep.Imported, err
}

// ImportWithOptions 是报告化导入入口,供 CLI dry-run/backup 使用。
func (e *Engine) ImportWithOptions(r io.Reader, opts ImportOptions) (ImportReport, error) {
	var rep ImportReport
	gz, err := gzip.NewReader(r)
	if err != nil {
		return rep, fmt.Errorf("解压失败(不是有效的 .kbundle?):%w", err)
	}
	defer gz.Close()
	limit := opts.MaxEntryBytes
	if limit <= 0 {
		limit = maxImportEntryBytes
	}
	if opts.Backup && !opts.DryRun {
		var buf bytes.Buffer
		if err := e.Export(&buf); err != nil {
			return rep, fmt.Errorf("导入前备份失败:%w", err)
		}
		rel := "local/import-backups/import-" + e.now().UTC().Format("20060102T150405Z") + ".kbundle"
		if err := e.Store.WriteKnowledgeFile(rel, buf.Bytes()); err != nil {
			return rep, fmt.Errorf("写入导入前备份失败:%w", err)
		}
		rep.BackupPath = ".knowledge/" + rel
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return rep, err
		}
		rep.Scanned++
		if hdr.Typeflag != tar.TypeReg {
			rep.Skipped++
			rep.Entries = append(rep.Entries, ImportEntry{Name: hdr.Name, Action: "skip", Reason: "非普通文件"})
			continue
		}
		clean, ok := importableBundleEntry(hdr.Name)
		if !ok {
			rep.Skipped++
			rep.Entries = append(rep.Entries, ImportEntry{Name: hdr.Name, Action: "skip", Reason: "不属于可导入知识路径"})
			continue
		}
		if hdr.Size > limit {
			rep.Skipped++
			rep.Entries = append(rep.Entries, ImportEntry{Name: clean, Action: "skip", Reason: fmt.Sprintf("单文件超过上限 %d bytes", limit), Bytes: hdr.Size})
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, limit+1))
		if err != nil {
			return rep, err
		}
		if int64(len(data)) > limit {
			return rep, fmt.Errorf("bundle 条目 %s 超过上限 %d bytes", clean, limit)
		}
		// 路径前缀重映射(tree shards 的 Node-ID 与文件名都要改)。
		if len(opts.PathRemap) > 0 && (strings.HasPrefix(clean, "tree/") || strings.HasPrefix(clean, "journal/")) {
			data = remapBytes(data, opts.PathRemap)
		}
		rep.Imported++
		rep.Bytes += int64(len(data))
		rep.Entries = append(rep.Entries, ImportEntry{Name: clean, Action: "import", Bytes: int64(len(data))})
		if opts.DryRun {
			continue
		}
		if err := e.Store.WriteKnowledgeFile(clean, data); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

func importableBundleEntry(name string) (string, bool) {
	clean := path.Clean(strings.TrimPrefix(name, "./"))
	if clean == "." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") ||
		strings.Contains(clean, "\\") || strings.Contains(clean, ":") {
		return "", false
	}
	if clean == "MANIFEST.json" {
		return "", false
	}
	if clean == "config.yaml" {
		return clean, true
	}
	switch {
	case strings.HasPrefix(clean, "tree/"):
		return clean, strings.HasSuffix(clean, ".yaml")
	case strings.HasPrefix(clean, "journal/"):
		return clean, strings.HasSuffix(clean, ".jsonl")
	case strings.HasPrefix(clean, "flows/"), strings.HasPrefix(clean, "topics/"):
		return clean, strings.HasSuffix(clean, ".yaml") || strings.HasSuffix(clean, ".jsonl")
	default:
		return "", false
	}
}

func writeTarFile(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
		ModTime:  time.Now(),
	}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// remapBytes 对 YAML/JSONL 内容做路径前缀替换(粗粒度字符串替换:Node-ID、
// ShardRel 里出现的路径前缀)。近似但够用——精确需解析每条 Node-ID 再判断,
// 对"种仓"场景过度。只替换 pathRemap 里的明确映射。
func remapBytes(data []byte, pathRemap map[string]string) []byte {
	s := string(data)
	for from, to := range pathRemap {
		s = strings.ReplaceAll(s, from, to)
	}
	return []byte(s)
}
