package engine

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
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

// Import 从 r 读 tar.gz bundle,解包到目标 .knowledge/ 的 tree+journal+flows+topics。
// pathRemap 可选:把 Node-ID 的路径前缀 from 映射到 to(跨仓迁移,如 "internal/auth/" → "pkg/auth/")。
// 已存在的文件会被覆盖(导入是合并语义:同名 shard 后者覆盖前者)。
// 不碰源码(铁律二),只写目标 .knowledge/。
func (e *Engine) Import(r io.Reader, pathRemap map[string]string) (int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("解压失败(不是有效的 .kbundle?):%w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	dir := e.Store.Dir()
	count := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// 安全:只解包到 .knowledge/ 内,防路径穿越(tar slip)。
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(hdr.Name) {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(clean))
		// 再次确认 target 在 dir 内(双保险)。
		if !strings.HasPrefix(filepath.Clean(target)+string(filepath.Separator), filepath.Clean(dir)+string(filepath.Separator)) &&
			filepath.Clean(target) != filepath.Clean(dir) {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return count, err
		}
		// 路径前缀重映射(tree shards 的 Node-ID 与文件名都要改)。
		if len(pathRemap) > 0 && (strings.HasPrefix(clean, "tree/") || strings.HasPrefix(clean, "journal/")) {
			data = remapBytes(data, pathRemap)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return count, err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
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
