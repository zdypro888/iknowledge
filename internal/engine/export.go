package engine

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/zdypro888/iknowledge/internal/model"
	"github.com/zdypro888/iknowledge/internal/store"
)

// R29 批次4 知识导出/导入(impl §留痕:纯 stdlib archive/tar+compress/gzip,零新依赖)。
// bundle 保留 tree/journal/flows/topics/config，排除 local/wip。

// Export 把 .knowledge/ 的知识制品打包成 tar.gz。tar/gzip 的 Close 会 flush
// 尾块和校验和，因此其错误与 Walk/Write 错误同等重要，不能由 defer 吞掉。
func (e *Engine) Export(w io.Writer) error {
	type exportFile struct {
		name string
		data []byte
	}
	var files []exportFile
	dir := e.Store.Dir()
	if err := filepath.WalkDir(dir, func(filePath string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if filePath == filepath.Join(dir, "local") || filePath == filepath.Join(dir, "wip") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("export 拒绝 knowledge symlink:%s", filePath)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("export 拒绝非普通 knowledge 文件:%s", filePath)
		}
		rel, err := filepath.Rel(dir, filePath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !isKnowledgeDataPath(rel) {
			return nil
		}
		if err := validatePortableBundlePath(rel); err != nil {
			return fmt.Errorf("export 路径 %q 不可便携: %w", rel, err)
		}
		data, err := e.Store.ReadKnowledgeFile(rel)
		if err != nil {
			return err
		}
		files = append(files, exportFile{name: rel, data: data})
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	foldedPaths := make(map[string]string, len(files))
	for _, file := range files {
		key := portableFoldKey(file.name)
		if previous := foldedPaths[key]; previous != "" {
			return fmt.Errorf("export 路径大小写冲突:%s 与 %s", previous, file.name)
		}
		foldedPaths[key] = file.name
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	manifest, err := json.Marshal(bundleManifest{
		Schema:     bundleManifestSchema,
		ExportedAt: e.now().UTC(),
		Repo:       filepath.ToSlash(e.Store.RepoRoot()),
	})
	if err != nil {
		return fmt.Errorf("编码 bundle manifest: %w", err)
	}
	var opErr error
	if err := writeTarFile(tw, "MANIFEST.json", manifest); err != nil {
		opErr = err
	} else {
		for _, file := range files {
			if err := writeTarFile(tw, file.name, file.data); err != nil {
				opErr = err
				break
			}
		}
	}
	twErr := tw.Close()
	gzErr := gz.Close()
	return errors.Join(opErr, wrapCloseErr("tar", twErr), wrapCloseErr("gzip", gzErr))
}

func wrapCloseErr(name string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("关闭 %s writer: %w", name, err)
}

const (
	bundleManifestSchema = 1
	maxImportEntryBytes  = 16 << 20
	maxImportTotalBytes  = 256 << 20
	maxImportHeaders     = 10000
)

var errImportDecompressedStreamLimit = errors.New("bundle 解压 tar stream 超过 hard cap")

// importStreamReader 给 tar header/PAX/GNU metadata 也加总解压边界。只累加
// hdr.Size 会漏掉 archive/tar 在 Next 内部消费的 PAX 扩展头。
type importStreamReader struct {
	r    io.Reader
	seen int64
	max  int64
}

func (r *importStreamReader) Read(p []byte) (int, error) {
	remaining := r.max - r.seen
	if remaining <= 0 {
		var probe [1]byte
		n, err := r.r.Read(probe[:])
		if n > 0 {
			return 0, errImportDecompressedStreamLimit
		}
		return 0, err
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.r.Read(p)
	r.seen += int64(n)
	return n, err
}

type bundleManifest struct {
	Schema     int       `json:"schema"`
	ExportedAt time.Time `json:"exported_at"`
	Repo       string    `json:"repo"`
}

type ImportOptions struct {
	PathRemap     map[string]string
	DryRun        bool
	Backup        bool
	Force         bool
	MaxEntryBytes int64
	MaxTotalBytes int64
	MaxHeaders    int
}

type ImportReport struct {
	Scanned    int
	Imported   int
	Skipped    int
	Redacted   int
	Bytes      int64
	BackupPath string
	Entries    []ImportEntry
}

type ImportEntry struct {
	Name     string
	Action   string
	Reason   string
	Bytes    int64
	Redacted int
}

func (r ImportReport) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "导入报告: scanned=%d importable=%d skipped=%d redacted=%d bytes=%d", r.Scanned, r.Imported, r.Skipped, r.Redacted, r.Bytes)
	if r.BackupPath != "" {
		fmt.Fprintf(&b, "\n备份: %s", r.BackupPath)
	}
	shown := r.Entries
	if len(shown) > 20 {
		shown = shown[:20]
	}
	for _, ent := range shown {
		switch ent.Action {
		case "import":
			fmt.Fprintf(&b, "\n  + %s (%d bytes", ent.Name, ent.Bytes)
			if ent.Redacted > 0 {
				fmt.Fprintf(&b, ", 脱敏 %d 处", ent.Redacted)
			}
			b.WriteString(")")
		case "replace":
			fmt.Fprintf(&b, "\n  ! %s (强制替换, %d bytes", ent.Name, ent.Bytes)
			if ent.Redacted > 0 {
				fmt.Fprintf(&b, ", 脱敏 %d 处", ent.Redacted)
			}
			b.WriteString(")")
		default:
			fmt.Fprintf(&b, "\n  - %s (%s)", ent.Name, ent.Reason)
		}
	}
	if len(r.Entries) > len(shown) {
		fmt.Fprintf(&b, "\n  ... 另有 %d 条", len(r.Entries)-len(shown))
	}
	return b.String()
}

func (e *Engine) Import(r io.Reader, pathRemap map[string]string) (int, error) {
	rep, err := e.ImportWithOptions(r, ImportOptions{PathRemap: pathRemap})
	return rep.Imported, err
}

type stagedImportFile struct {
	name     string
	data     []byte
	redacted int
}

// ImportWithOptions 先把整个 bundle 读入 staging、做语义 remap 和最终态冲突验证，
// 验证通过后才开始写盘。首个真相写之前持久化全部 before-image WAL；
// 进程在任意一个 rename 后崩溃，下次 reload/import 也会整体回滚，不只是本进程内尽力修复。
func (e *Engine) ImportWithOptions(r io.Reader, opts ImportOptions) (ImportReport, error) {
	var rep ImportReport
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	recovered, err := e.Store.RecoverTruthTransactionWithStatus()
	if err != nil {
		return rep, fmt.Errorf("导入前恢复未完成事务:%w", err)
	}
	if recovered {
		e.rt.cache = nil // 旧 cache 可能是半应用字节的同 mtime/size 快照
	}
	mapper, err := newImportPathMapper(opts.PathRemap)
	if err != nil {
		return rep, err
	}
	entryLimit := opts.MaxEntryBytes
	if entryLimit <= 0 {
		entryLimit = maxImportEntryBytes
	}
	totalLimit := opts.MaxTotalBytes
	if totalLimit <= 0 {
		totalLimit = maxImportTotalBytes
	}
	headerLimit := opts.MaxHeaders
	if headerLimit <= 0 {
		headerLimit = maxImportHeaders
	}
	if entryLimit > maxImportTotalBytes || totalLimit > maxImportTotalBytes || headerLimit > maxImportHeaders {
		return rep, fmt.Errorf("bundle 上限不得超过 hard cap: entry/total=%d bytes, headers=%d", maxImportTotalBytes, maxImportHeaders)
	}
	if totalLimit < entryLimit {
		return rep, fmt.Errorf("总 staging 上限 %d 小于单条上限 %d", totalLimit, entryLimit)
	}

	compressed := bufio.NewReader(r)
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return rep, fmt.Errorf("解压失败(不是有效的 .kbundle?):%w", err)
	}
	// 默认的 gzip.Reader 会把紧随其后的第二个 member 当成同一输入；
	// bundle 必须只有一个 member，否则攻击者可在第一个 tar EOF 后藏数据。
	gz.Multistream(false)
	// 每个逻辑 header 额外预留 10 KiB PAX/GNU metadata，再给 tar 尾块 64 KiB。
	// 这个边界覆盖 payload 之外的原始解压字节，防止扩展头绕过 hdr.Size 总和。
	stream := &importStreamReader{r: gz, max: totalLimit + int64(headerLimit)*(10<<10) + (64 << 10)}
	tr := tar.NewReader(stream)
	var staged []stagedImportFile
	outputNames := map[string]string{} // remap 后路径 -> bundle 原路径
	outputFoldNames := map[string]string{}
	var declaredTotal, stagedTotal int64
	manifestSeen := false
	for {
		hdr, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			_ = gz.Close()
			return rep, nextErr
		}
		rep.Scanned++
		if rep.Scanned > headerLimit {
			_ = gz.Close()
			return rep, fmt.Errorf("bundle header 数超过上限 %d", headerLimit)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			_ = gz.Close()
			return rep, fmt.Errorf("bundle 条目 %q 非普通文件(type=%d)", hdr.Name, hdr.Typeflag)
		}
		if err := validatePortableBundlePath(hdr.Name); err != nil {
			_ = gz.Close()
			return rep, fmt.Errorf("bundle 条目路径 %q 不可便携: %w", hdr.Name, err)
		}
		clean, ok := importableBundleEntry(hdr.Name)
		isManifest := hdr.Name == "MANIFEST.json"
		if !ok && !isManifest {
			_ = gz.Close()
			return rep, fmt.Errorf("bundle 条目 %q 不属于可导入知识路径", hdr.Name)
		}
		if hdr.Size < 0 || hdr.Size > entryLimit {
			_ = gz.Close()
			return rep, fmt.Errorf("bundle 条目 %q 声明大小 %d 超过单文件上限 %d", hdr.Name, hdr.Size, entryLimit)
		}
		if hdr.Size > totalLimit-declaredTotal {
			_ = gz.Close()
			return rep, fmt.Errorf("bundle 声明解压总量超过上限 %d bytes", totalLimit)
		}
		declaredTotal += hdr.Size
		data, readErr := io.ReadAll(tr)
		if readErr != nil {
			_ = gz.Close()
			return rep, readErr
		}
		if int64(len(data)) != hdr.Size {
			_ = gz.Close()
			return rep, fmt.Errorf("bundle 条目 %q 实际大小 %d != 声明 %d", hdr.Name, len(data), hdr.Size)
		}
		if isManifest {
			if manifestSeen {
				_ = gz.Close()
				return rep, fmt.Errorf("bundle 含多个 MANIFEST.json")
			}
			if err := validateBundleManifest(data); err != nil {
				_ = gz.Close()
				return rep, err
			}
			manifestSeen = true
			continue
		}
		outName, outData, transformErr := transformImportFile(clean, data, mapper)
		if transformErr != nil {
			_ = gz.Close()
			return rep, fmt.Errorf("bundle 条目 %s: %w", clean, transformErr)
		}
		if err := validatePortableBundlePath(outName); err != nil {
			_ = gz.Close()
			return rep, fmt.Errorf("bundle 条目 %s remap 后路径 %q 不可便携: %w", clean, outName, err)
		}
		// bundle 来自仓库外部，不能依赖当前 Go 模型的 redact 标签。对完成
		// remap 的 YAML/JSONL 原文统一脱敏，dry-run 与真实导入走同一数据。
		redactedText, redaction := RedactText(string(outData))
		outData = []byte(redactedText)
		rep.Redacted += redaction.Count
		if prev := outputNames[outName]; prev != "" {
			_ = gz.Close()
			return rep, fmt.Errorf("导入输出路径冲突:%s 与 %s 均映射到 %s", prev, clean, outName)
		}
		foldKey := portableFoldKey(outName)
		if previous := outputFoldNames[foldKey]; previous != "" {
			_ = gz.Close()
			return rep, fmt.Errorf("导入输出路径大小写冲突:%s(%s) 与 %s(%s)", previous, outputNames[previous], outName, clean)
		}
		if int64(len(outData)) > totalLimit-stagedTotal {
			_ = gz.Close()
			return rep, fmt.Errorf("bundle remap 后 staging 总量超过上限 %d bytes", totalLimit)
		}
		stagedTotal += int64(len(outData))
		outputNames[outName] = clean
		outputFoldNames[foldKey] = outName
		staged = append(staged, stagedImportFile{name: outName, data: outData, redacted: redaction.Count})
	}
	if !manifestSeen {
		_ = gz.Close()
		return rep, fmt.Errorf("bundle 缺 MANIFEST.json")
	}
	// tar EOF 之后只允许 tar 规范的零填充。超量零填充同样受总量上限约束，
	// 非零字节意味着归档 EOF 后藏了未声明数据。
	padding, err := io.ReadAll(io.LimitReader(stream, totalLimit-declaredTotal+1))
	if err != nil {
		_ = gz.Close()
		return rep, fmt.Errorf("校验 gzip trailer: %w", err)
	}
	if int64(len(padding)) > totalLimit-declaredTotal {
		_ = gz.Close()
		return rep, fmt.Errorf("tar EOF 后填充使解压总量超过上限 %d bytes", totalLimit)
	}
	for _, b := range padding {
		if b != 0 {
			_ = gz.Close()
			return rep, fmt.Errorf("tar EOF 后含非零隐藏数据")
		}
	}
	if err := gz.Close(); err != nil {
		return rep, fmt.Errorf("关闭 gzip reader: %w", err)
	}
	if _, err := compressed.Peek(1); err == nil {
		return rep, fmt.Errorf("bundle 含第二个 gzip member 或压缩尾随数据")
	} else if !errors.Is(err, io.EOF) {
		return rep, fmt.Errorf("校验 gzip 压缩尾随:%w", err)
	}
	if err := e.mergeStagedJournals(staged); err != nil {
		return rep, err
	}
	stagedTotal = 0
	for _, file := range staged {
		if int64(len(file.data)) > totalLimit-stagedTotal {
			return rep, fmt.Errorf("journal 合并后 staging 总量超过上限 %d bytes", totalLimit)
		}
		stagedTotal += int64(len(file.data))
	}
	if err := e.preflightPortableTargets(staged); err != nil {
		return rep, err
	}

	// journal 有 append-only 合并语义；其他文件默认不覆盖。字节不同但 YAML/JSONL
	// 语义相同的目标可幂等跳过，真正不同必须由 --force 显式授权。
	writeStage := make([]stagedImportFile, 0, len(staged))
	for _, file := range staged {
		existing, readErr := e.Store.ReadKnowledgeFile(file.name)
		switch {
		case os.IsNotExist(readErr):
			writeStage = append(writeStage, file)
			rep.Imported++
			rep.Bytes += int64(len(file.data))
			rep.Entries = append(rep.Entries, ImportEntry{Name: file.name, Action: "import", Bytes: int64(len(file.data)), Redacted: file.redacted})
		case readErr != nil:
			return rep, fmt.Errorf("读取导入目标 %s: %w", file.name, readErr)
		case strings.HasPrefix(file.name, "journal/"):
			if bytes.Equal(existing, file.data) {
				rep.Skipped++
				rep.Entries = append(rep.Entries, ImportEntry{Name: file.name, Action: "skip", Reason: "journal 无新记录"})
				continue
			}
			writeStage = append(writeStage, file)
			rep.Imported++
			rep.Bytes += int64(len(file.data))
			rep.Entries = append(rep.Entries, ImportEntry{Name: file.name, Action: "import", Reason: "journal 合并", Bytes: int64(len(file.data)), Redacted: file.redacted})
		default:
			equal, err := importSemanticEqual(file.name, existing, file.data)
			if err != nil {
				return rep, fmt.Errorf("比较导入目标 %s: %w", file.name, err)
			}
			if equal {
				rep.Skipped++
				rep.Entries = append(rep.Entries, ImportEntry{Name: file.name, Action: "skip", Reason: "与现有文件语义相同"})
				continue
			}
			if !opts.Force {
				return rep, fmt.Errorf("导入默认不覆盖已有不同文件 %s；核对 dry-run 报告后显式加 --force", file.name)
			}
			writeStage = append(writeStage, file)
			rep.Imported++
			rep.Bytes += int64(len(file.data))
			rep.Entries = append(rep.Entries, ImportEntry{Name: file.name, Action: "replace", Reason: "--force", Bytes: int64(len(file.data)), Redacted: file.redacted})
		}
	}
	if err := e.validateImportStage(writeStage); err != nil {
		return rep, err
	}
	if opts.DryRun {
		return rep, nil
	}

	if opts.Backup {
		var buf bytes.Buffer
		if err := e.Export(&buf); err != nil {
			return rep, fmt.Errorf("导入前备份失败:%w", err)
		}
		rel := "local/import-backups/import-" + e.now().UTC().Format("20060102T150405Z") + "-" + model.NewEntryID() + ".kbundle"
		if err := e.Store.WriteKnowledgeFile(rel, buf.Bytes()); err != nil {
			return rep, fmt.Errorf("写入导入前备份失败:%w", err)
		}
		rep.BackupPath = ".knowledge/" + rel
	}

	staged = writeStage
	sort.Slice(staged, func(i, j int) bool { return staged[i].name < staged[j].name })
	if len(staged) == 0 {
		return rep, nil
	}
	rels := make(map[string]bool, len(staged))
	for _, file := range staged {
		rels[file.name] = true
	}
	tx, err := e.prepareTruthTransactionLocked(rels)
	if err != nil {
		return rep, fmt.Errorf("准备导入事务 WAL:%w", err)
	}
	defer e.guardTruthTransactionPanicLocked(tx)
	rollback := func(cause error) error {
		return e.rollbackTruthTransactionLocked(tx, cause)
	}
	for _, file := range staged {
		if err := e.Store.WriteKnowledgeFile(file.name, file.data); err != nil {
			return rep, rollback(fmt.Errorf("写入导入目标 %s: %w", file.name, err))
		}
		if e.afterImportTruthWrite != nil {
			if err := e.afterImportTruthWrite(file.name); err != nil {
				return rep, rollback(fmt.Errorf("导入 truth write 后检查 %s:%w", file.name, err))
			}
		}
	}
	committed, commitErr := e.commitTruthTransactionLocked(tx)
	if !committed {
		return rep, rollback(fmt.Errorf("导入写 committed marker:%w", commitErr))
	}
	if commitErr != nil {
		return rep, fmt.Errorf("导入已提交但 WAL 清理失败(不要重试同一操作):%w", commitErr)
	}
	return rep, nil
}

func validateBundleManifest(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var manifest bundleManifest
	if err := dec.Decode(&manifest); err != nil {
		return fmt.Errorf("解析 MANIFEST.json: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("MANIFEST.json 含多个 JSON 值")
		}
		return fmt.Errorf("解析 MANIFEST.json 尾随: %w", err)
	}
	if manifest.Schema != bundleManifestSchema {
		return fmt.Errorf("MANIFEST.json schema=%d，只支持 %d", manifest.Schema, bundleManifestSchema)
	}
	if manifest.ExportedAt.IsZero() {
		return fmt.Errorf("MANIFEST.json 缺有效 exported_at")
	}
	if strings.TrimSpace(manifest.Repo) == "" || !utf8.ValidString(manifest.Repo) {
		return fmt.Errorf("MANIFEST.json 缺有效 repo")
	}
	return nil
}

// validatePortableBundlePath 取 Windows/macOS/Linux 的交集，避免 bundle 在开发机
// 上看似是两个文件，到大小写不敏感或 Windows 目标上却覆盖同一路径。
// 标准库没有 Unicode NFC 归一化，因此保守拒绝组合标记：预组合字符可用，
// 分解组合形不会与它形成跨平台隐性碰撞。
func validatePortableBundlePath(name string) error {
	if name == "" || !utf8.ValidString(name) || len(name) > 4096 {
		return fmt.Errorf("空路径、非 UTF-8 或路径过长")
	}
	if strings.Contains(name, "\\") || strings.Contains(name, ":") || strings.HasPrefix(name, "/") || path.Clean(name) != name {
		return fmt.Errorf("必须是规范的正斜杠相对路径")
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r) || unicode.Is(unicode.Me, r) {
			return fmt.Errorf("含控制符或 Unicode 组合标记")
		}
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || len(component) > 255 || strings.ContainsAny(component, `<>"|?*`) ||
			strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return fmt.Errorf("路径分量为空、过长或以点/空格结尾")
		}
		base := component
		if dot := strings.IndexByte(base, '.'); dot >= 0 {
			base = base[:dot]
		}
		upper := strings.ToUpper(base)
		reserved := upper == "CON" || upper == "PRN" || upper == "AUX" || upper == "NUL"
		if len(upper) == 4 && (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) && upper[3] >= '1' && upper[3] <= '9' {
			reserved = true
		}
		if reserved {
			return fmt.Errorf("含 Windows 保留名 %q", component)
		}
	}
	return nil
}

func (e *Engine) preflightPortableTargets(staged []stagedImportFile) error {
	existing := map[string]string{}
	if err := filepath.WalkDir(e.Store.Dir(), func(filePath string, d os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			if filePath == filepath.Join(e.Store.Dir(), "local") || filePath == filepath.Join(e.Store.Dir(), "wip") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("导入预检拒绝 knowledge symlink:%s", filePath)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("导入预检拒绝 knowledge 非普通文件:%s", filePath)
		}
		rel, err := filepath.Rel(e.Store.Dir(), filePath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !isKnowledgeDataPath(rel) {
			return nil
		}
		if err := validatePortableBundlePath(rel); err != nil {
			return fmt.Errorf("现有 knowledge 路径 %q 不可便携: %w", rel, err)
		}
		key := portableFoldKey(rel)
		if prev := existing[key]; prev != "" {
			return fmt.Errorf("现有 knowledge 路径大小写冲突:%s 与 %s", prev, rel)
		}
		existing[key] = rel
		return nil
	}); err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, file := range staged {
		if current := existing[portableFoldKey(file.name)]; current != "" && file.name != current {
			return fmt.Errorf("导入输出 %s 与现有路径 %s 仅大小写不同，为避免便携覆盖已拒绝", file.name, current)
		}
	}
	return nil
}

// portableFoldKey 为 Unicode simple-fold 等价类选最小 rune；这与
// strings.EqualFold 的逐 rune 语义一致，但可用 map 把碰撞检查从 O(N²) 降为 O(N)。
func portableFoldKey(value string) string {
	var b strings.Builder
	for _, r := range value {
		minimum := r
		for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
			if folded < minimum {
				minimum = folded
			}
		}
		b.WriteRune(minimum)
	}
	return b.String()
}

func isKnowledgeDataPath(rel string) bool {
	clean, ok := importableBundleEntry(rel)
	return ok && clean == rel
}

func importSemanticEqual(name string, a, b []byte) (bool, error) {
	if strings.HasSuffix(name, ".jsonl") {
		decode := func(data []byte) ([]any, error) {
			var values []any
			for lineNo, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				dec := json.NewDecoder(strings.NewReader(line))
				dec.UseNumber()
				var value any
				if err := dec.Decode(&value); err != nil {
					return nil, fmt.Errorf("第 %d 行 JSON: %w", lineNo+1, err)
				}
				if err := ensureJSONEOF(dec); err != nil {
					return nil, fmt.Errorf("第 %d 行 JSON: %w", lineNo+1, err)
				}
				values = append(values, value)
			}
			return values, nil
		}
		av, err := decode(a)
		if err != nil {
			return false, err
		}
		bv, err := decode(b)
		if err != nil {
			return false, err
		}
		return reflect.DeepEqual(av, bv), nil
	}
	var av, bv any
	if err := yaml.Unmarshal(a, &av); err != nil {
		return false, err
	}
	if err := yaml.Unmarshal(b, &bv); err != nil {
		return false, err
	}
	return reflect.DeepEqual(av, bv), nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("含多个 JSON 值")
		}
		return err
	}
	return nil
}

func (e *Engine) mergeStagedJournals(staged []stagedImportFile) error {
	for i := range staged {
		if !strings.HasPrefix(staged[i].name, "journal/") {
			continue
		}
		existing, err := e.Store.ReadKnowledgeFile(staged[i].name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("读取现有 journal %s: %w", staged[i].name, err)
		}
		merged, err := mergeJournalBytes(existing, staged[i].data)
		if err != nil {
			return fmt.Errorf("合并 journal %s: %w", staged[i].name, err)
		}
		staged[i].data = merged
	}
	return nil
}

func mergeJournalBytes(existing, incoming []byte) ([]byte, error) {
	var out bytes.Buffer
	byID := map[string]string{} // ID -> canonical JSON
	seenCanonical := map[string]bool{}
	add := func(data []byte, source string, strict bool) error {
		for lineNo, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			canonical, id, err := canonicalJournalLine(line)
			if err != nil || id == "" {
				if strict {
					if err == nil {
						err = fmt.Errorf("change ID 为空")
					}
					return fmt.Errorf("%s 第 %d 行:%w", source, lineNo+1, err)
				}
				// 旧库坏行属于 store 容错契约；保留原行，但 bundle 新行必须严格。
				out.WriteString(line)
				out.WriteByte('\n')
				continue
			}
			if prev := byID[id]; prev != "" && prev != canonical {
				return fmt.Errorf("change ID %s 同 ID 不同内容(%s 第 %d 行)", id, source, lineNo+1)
			}
			if seenCanonical[canonical] {
				continue
			}
			byID[id] = canonical
			seenCanonical[canonical] = true
			out.WriteString(line)
			out.WriteByte('\n')
		}
		return nil
	}
	if err := add(existing, "目标仓", false); err != nil {
		return nil, err
	}
	if err := add(incoming, "bundle", true); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func canonicalJournalLine(line string) (canonical, id string, err error) {
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	var value any
	if err = dec.Decode(&value); err != nil {
		return "", "", err
	}
	if err = ensureJSONEOF(dec); err != nil {
		return "", "", err
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return "", "", fmt.Errorf("journal 行不是 JSON object")
	}
	id, _ = obj["id"].(string)
	encoded, err := json.Marshal(value)
	return string(encoded), id, err
}

func importableBundleEntry(name string) (string, bool) {
	clean := path.Clean(name)
	if clean == "." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") ||
		clean != name || strings.Contains(clean, "\\") || strings.Contains(clean, ":") {
		return "", false
	}
	if clean == "MANIFEST.json" {
		return "", false
	}
	if clean == "config.yaml" || clean == "project.yaml" {
		return clean, true
	}
	switch {
	case strings.HasPrefix(clean, "tree/"):
		return clean, strings.HasSuffix(clean, ".yaml")
	case strings.HasPrefix(clean, "journal/"):
		if strings.Count(clean, "/") != 1 || !strings.HasSuffix(clean, ".jsonl") {
			return "", false
		}
		month := strings.TrimSuffix(strings.TrimPrefix(clean, "journal/"), ".jsonl")
		parsed, err := time.Parse("2006-01", month)
		return clean, err == nil && parsed.Format("2006-01") == month
	case strings.HasPrefix(clean, "flows/"), strings.HasPrefix(clean, "topics/"):
		// 运行时 LoadFlows 的正式布局是一对象一文件、目录顶层 YAML。
		// 不接受“能导入却永远不会被加载”的嵌套路径或 JSONL。
		return clean, strings.Count(clean, "/") == 1 && strings.HasSuffix(clean, ".yaml")
	default:
		return "", false
	}
}

func writeTarFile(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg, ModTime: time.Now()}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

type importPathRule struct {
	from string
	to   string
}

type importPathMapper struct {
	rules []importPathRule
}

func newImportPathMapper(raw map[string]string) (importPathMapper, error) {
	var m importPathMapper
	for from, to := range raw {
		from = strings.TrimSuffix(strings.TrimSpace(from), "/")
		to = strings.TrimSuffix(strings.TrimSpace(to), "/")
		if from == "" || to == "" {
			return m, fmt.Errorf("非法 --remap %q=%q:from/to 均须为非空 repo 相对路径", from, to)
		}
		if _, ok := model.SafeRel(from); !ok {
			return m, fmt.Errorf("非法 remap.from %q", from)
		}
		if _, ok := model.SafeRel(to); !ok {
			return m, fmt.Errorf("非法 remap.to %q", to)
		}
		if strings.ContainsAny(from+to, "*?[") {
			return m, fmt.Errorf("非法 remap %q=%q:前缀是字面路径，不能含 glob 元字符", from, to)
		}
		if err := validatePortableBundlePath(from); err != nil {
			return m, fmt.Errorf("非法 remap.from %q:%w", from, err)
		}
		if err := validatePortableBundlePath(to); err != nil {
			return m, fmt.Errorf("非法 remap.to %q:%w", to, err)
		}
		m.rules = append(m.rules, importPathRule{from: from, to: to})
	}
	// 最长前缀优先；每个值只匹配一次，A→B、B→C 不会级联成 A→C。
	sort.Slice(m.rules, func(i, j int) bool {
		if len(m.rules[i].from) != len(m.rules[j].from) {
			return len(m.rules[i].from) > len(m.rules[j].from)
		}
		return m.rules[i].from < m.rules[j].from
	})
	return m, nil
}

func (m importPathMapper) repoPath(value string) (string, error) {
	if value == "." {
		return value, nil
	}
	trailing := strings.HasSuffix(value, "/")
	base := strings.TrimSuffix(value, "/")
	if _, ok := model.SafeRel(base); !ok {
		return "", fmt.Errorf("非法 repo 相对路径 %q", value)
	}
	original := base
	for _, rule := range m.rules {
		if original == rule.from || strings.HasPrefix(original, rule.from+"/") {
			base = rule.to + strings.TrimPrefix(original, rule.from)
			break
		}
	}
	if _, ok := model.SafeRel(base); !ok {
		return "", fmt.Errorf("remap 后路径非法 %q", base)
	}
	if err := validatePortableBundlePath(base); err != nil {
		return "", fmt.Errorf("remap 后 repo 路径 %q 不可便携:%w", base, err)
	}
	if trailing {
		base += "/"
	}
	return base, nil
}

func (m importPathMapper) nodeID(value string) (string, error) {
	file, symbol := model.SplitNodeID(value)
	mapped, err := m.repoPath(file)
	if err != nil {
		return "", err
	}
	if symbol != "" {
		mapped += "#" + symbol
	}
	if !model.SafeNodeID(mapped) {
		return "", fmt.Errorf("remap 后 Node ID 非法 %q", mapped)
	}
	return mapped, nil
}

func (m importPathMapper) entryRef(value string) (string, error) {
	i := strings.LastIndexByte(value, '#')
	if i <= 0 || i == len(value)-1 {
		return "", fmt.Errorf("非法 entry 引用 %q", value)
	}
	node, err := m.nodeID(value[:i])
	if err != nil {
		return "", err
	}
	return node + value[i:], nil
}

func transformImportFile(name string, data []byte, mapper importPathMapper) (string, []byte, error) {
	switch {
	case name == "project.yaml":
		out, err := transformProjectYAML(data, mapper)
		return name, out, err
	case strings.HasPrefix(name, "tree/"):
		inner := strings.TrimSuffix(strings.TrimPrefix(name, "tree/"), ".yaml")
		mapped, err := mapper.repoPath(inner)
		if err != nil {
			return "", nil, err
		}
		out, err := transformTreeYAML(data, mapper)
		return "tree/" + mapped + ".yaml", out, err
	case strings.HasPrefix(name, "journal/"):
		out, err := transformJournalJSONL(data, mapper)
		return name, out, err
	case strings.HasPrefix(name, "flows/"), strings.HasPrefix(name, "topics/"):
		if strings.HasSuffix(name, ".yaml") {
			out, err := transformFlowYAML(data, mapper)
			return name, out, err
		}
		out, err := transformFlowJSONL(data, mapper)
		return name, out, err
	case name == "config.yaml":
		out, err := transformImportConfig(data, mapper)
		return name, out, err
	default:
		return name, data, nil
	}
}

func validateImportConfig(data []byte) error {
	var cfg store.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("解析 config.yaml:%w", err)
	}
	return store.ValidateConfig(&cfg)
}

func transformImportConfig(data []byte, mapper importPathMapper) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("解析 config.yaml:%w", err)
	}
	doc, err := yamlDocumentMap(&root)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"include", "exclude"} {
		seq := yamlMapValue(doc, key)
		if seq == nil {
			continue
		}
		if seq.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("config.%s 必须是 sequence", key)
		}
		for _, item := range seq.Content {
			if item.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("config.%s 条目必须是 scalar", key)
			}
			mapped, err := mapper.glob(item.Value)
			if err != nil {
				return nil, fmt.Errorf("config.%s pattern %q:%w", key, item.Value, err)
			}
			item.Value = mapped
		}
	}
	out, err := yaml.Marshal(&root)
	if err != nil {
		return nil, err
	}
	if err := validateImportConfig(out); err != nil {
		return nil, err
	}
	return out, nil
}

func (m importPathMapper) glob(pattern string) (string, error) {
	if pattern == "" || strings.Contains(pattern, "\\") {
		return "", fmt.Errorf("空 pattern 或反斜杠转义无法安全跨平台变换")
	}
	if _, err := path.Match(pattern, "probe"); err != nil {
		return "", err
	}
	meta := strings.IndexAny(pattern, "*?[")
	if meta < 0 {
		return m.repoPath(pattern)
	}
	if len(m.rules) == 0 {
		return pattern, nil
	}
	literal := strings.TrimSuffix(pattern[:meta], "/")
	for _, rule := range m.rules {
		if literal == rule.from || strings.HasPrefix(literal, rule.from+"/") {
			if literal == rule.from && !strings.HasSuffix(pattern[:meta], "/") {
				return "", fmt.Errorf("glob 元字符紧跟 remap 前缀 %s，会同时匹配不会被 remap 的同名前缀路径", rule.from)
			}
			mappedPrefix := rule.to + strings.TrimPrefix(literal, rule.from)
			if strings.HasSuffix(pattern[:meta], "/") {
				mappedPrefix += "/"
			}
			mapped := mappedPrefix + pattern[meta:]
			if _, err := path.Match(mapped, "probe"); err != nil {
				return "", err
			}
			return mapped, nil
		}
		if literal == "" || rule.from == literal || strings.HasPrefix(rule.from, literal+"/") || strings.Contains(pattern[meta:], rule.from) {
			return "", fmt.Errorf("glob 匹配面跨越 remap %s=%s，无法用单个 path.Match pattern 等价表示", rule.from, rule.to)
		}
	}
	return pattern, nil
}

func yamlDocumentMap(root *yaml.Node) (*yaml.Node, error) {
	n := root
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("YAML 根不是 mapping")
	}
	return n, nil
}

func yamlMapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func transformScalar(n *yaml.Node, f func(string) (string, error)) error {
	if n == nil || n.Kind != yaml.ScalarNode {
		return fmt.Errorf("期望 scalar")
	}
	mapped, err := f(n.Value)
	if err != nil {
		return err
	}
	n.Value = mapped
	return nil
}

func transformScalarSeq(n *yaml.Node, f func(string) (string, error)) error {
	if n == nil {
		return nil
	}
	if n.Kind != yaml.SequenceNode {
		return fmt.Errorf("期望 sequence")
	}
	for _, item := range n.Content {
		if err := transformScalar(item, f); err != nil {
			return err
		}
	}
	return nil
}

func transformTreeYAML(data []byte, mapper importPathMapper) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	doc, err := yamlDocumentMap(&root)
	if err != nil {
		return nil, err
	}
	nodes := yamlMapValue(doc, "nodes")
	if nodes == nil || nodes.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("tree shard 缺 nodes sequence")
	}
	for _, node := range nodes.Content {
		if err := transformTreeNodeYAML(node, mapper); err != nil {
			return nil, err
		}
	}
	var sh store.Shard
	if err := root.Decode(&sh); err != nil {
		return nil, err
	}
	if sh.Schema > model.SchemaVersion {
		return nil, fmt.Errorf("tree shard schema %d > %d", sh.Schema, model.SchemaVersion)
	}
	return yaml.Marshal(&root)
}

func transformProjectYAML(data []byte, mapper importPathMapper) ([]byte, error) {
	return transformTreeYAML(data, mapper)
}

func transformTreeNodeYAML(node *yaml.Node, mapper importPathMapper) error {
	if node == nil || node.Kind != yaml.MappingNode {
		return fmt.Errorf("nodes 元素不是 mapping")
	}
	if err := transformScalar(yamlMapValue(node, "id"), mapper.nodeID); err != nil {
		return err
	}
	if anchor := yamlMapValue(node, "anchor"); anchor != nil {
		if file := yamlMapValue(anchor, "file"); file != nil {
			if err := transformScalar(file, mapper.repoPath); err != nil {
				return err
			}
		}
	}
	if err := transformScalarSeq(yamlMapValue(node, "lineage"), mapper.nodeID); err != nil {
		return err
	}
	entries := yamlMapValue(node, "entries")
	if entries != nil {
		if entries.Kind != yaml.SequenceNode {
			return fmt.Errorf("entries 不是 sequence")
		}
		for _, entry := range entries.Content {
			if err := transformScalarSeq(yamlMapValue(entry, "based_on"), mapper.entryRef); err != nil {
				return err
			}
			if err := transformScalarSeq(yamlMapValue(entry, "disputes"), mapper.entryRef); err != nil {
				return err
			}
		}
	}
	return nil
}

func transformRawNodeYAML(value string, mapper importPathMapper) (string, error) {
	if strings.TrimSpace(value) == "" {
		return value, nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(value), &root); err != nil {
		return "", err
	}
	node := &root
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = node.Content[0]
	}
	if err := transformTreeNodeYAML(node, mapper); err != nil {
		return "", err
	}
	data, err := yaml.Marshal(node)
	return string(data), err
}

func transformFlowYAML(data []byte, mapper importPathMapper) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	doc, err := yamlDocumentMap(&root)
	if err != nil {
		return nil, err
	}
	flow := yamlMapValue(doc, "flow")
	if flow == nil || flow.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("flow shard 缺 flow mapping")
	}
	if steps := yamlMapValue(flow, "steps"); steps != nil {
		if steps.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("flow.steps 不是 sequence")
		}
		for _, step := range steps.Content {
			if err := transformScalar(yamlMapValue(step, "node"), mapper.nodeID); err != nil {
				return nil, err
			}
		}
	}
	var shard model.FlowShard
	if err := root.Decode(&shard); err != nil || shard.Flow.ID == "" {
		return nil, fmt.Errorf("非法 flow shard")
	}
	if shard.Schema > model.SchemaVersion {
		return nil, fmt.Errorf("flow shard schema %d > %d", shard.Schema, model.SchemaVersion)
	}
	return yaml.Marshal(&root)
}

func transformJournalJSONL(data []byte, mapper importPathMapper) ([]byte, error) {
	var out bytes.Buffer
	for lineNo, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return nil, fmt.Errorf("journal 第 %d 行:%w", lineNo+1, err)
		}
		if err := remapRawStringSlice(obj, "nodes", mapper.nodeID); err != nil {
			return nil, err
		}
		if raw := obj["remaps"]; len(raw) > 0 {
			var remaps []map[string]json.RawMessage
			if err := json.Unmarshal(raw, &remaps); err != nil {
				return nil, err
			}
			for _, remap := range remaps {
				if err := remapRawString(remap, "from", mapper.nodeID); err != nil {
					return nil, err
				}
				if err := remapRawStringSlice(remap, "to", mapper.nodeID); err != nil {
					return nil, err
				}
				if entriesRaw := remap["entries"]; len(entriesRaw) > 0 {
					var entries map[string]string
					if err := json.Unmarshal(entriesRaw, &entries); err != nil {
						return nil, err
					}
					for id, dst := range entries {
						mapped, err := mapper.nodeID(dst)
						if err != nil {
							return nil, err
						}
						entries[id] = mapped
					}
					remap["entries"], _ = json.Marshal(entries)
				}
			}
			obj["remaps"], _ = json.Marshal(remaps)
		}
		for _, effectsKey := range []string{"effects"} {
			if raw := obj[effectsKey]; len(raw) > 0 {
				var effects []map[string]json.RawMessage
				if err := json.Unmarshal(raw, &effects); err != nil {
					return nil, err
				}
				for _, effect := range effects {
					if err := remapRawString(effect, "entry", mapper.entryRef); err != nil {
						return nil, err
					}
				}
				obj[effectsKey], _ = json.Marshal(effects)
			}
		}
		if raw := obj["node_effects"]; len(raw) > 0 {
			var effects []map[string]json.RawMessage
			if err := json.Unmarshal(raw, &effects); err != nil {
				return nil, err
			}
			for _, effect := range effects {
				if err := remapRawString(effect, "node", mapper.nodeID); err != nil {
					return nil, err
				}
				for _, key := range []string{"before_shard", "after_shard"} {
					if err := remapRawString(effect, key, mapper.shardRel); err != nil {
						return nil, err
					}
				}
				for _, key := range []string{"before", "after"} {
					if nodeRaw := effect[key]; len(nodeRaw) > 0 && string(nodeRaw) != "null" {
						mapped, err := remapNodeJSON(nodeRaw, mapper)
						if err != nil {
							return nil, err
						}
						effect[key] = mapped
					}
				}
				for _, key := range []string{"before_raw", "after_raw"} {
					if err := remapRawString(effect, key, func(value string) (string, error) {
						return transformRawNodeYAML(value, mapper)
					}); err != nil {
						return nil, err
					}
				}
			}
			obj["node_effects"], _ = json.Marshal(effects)
		}
		encoded, err := json.Marshal(obj)
		if err != nil {
			return nil, err
		}
		out.Write(encoded)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

func (m importPathMapper) shardRel(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !strings.HasPrefix(value, "tree/") || !strings.HasSuffix(value, ".yaml") {
		return "", fmt.Errorf("非法 node effect shard %q", value)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(value, "tree/"), ".yaml")
	mapped, err := m.repoPath(inner)
	if err != nil {
		return "", err
	}
	return "tree/" + mapped + ".yaml", nil
}

func rawKey(obj map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		if _, ok := obj[key]; ok {
			return key
		}
	}
	return ""
}

func remapNodeJSON(raw json.RawMessage, mapper importPathMapper) (json.RawMessage, error) {
	var node map[string]json.RawMessage
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	if key := rawKey(node, "id", "ID"); key != "" {
		if err := remapRawString(node, key, mapper.nodeID); err != nil {
			return nil, err
		}
	}
	if key := rawKey(node, "anchor", "Anchor"); key != "" {
		var anchor map[string]json.RawMessage
		if err := json.Unmarshal(node[key], &anchor); err != nil {
			return nil, err
		}
		if fileKey := rawKey(anchor, "file", "File"); fileKey != "" {
			if err := remapRawString(anchor, fileKey, mapper.repoPath); err != nil {
				return nil, err
			}
		}
		node[key], _ = json.Marshal(anchor)
	}
	if key := rawKey(node, "lineage", "Lineage"); key != "" {
		if err := remapRawStringSlice(node, key, mapper.nodeID); err != nil {
			return nil, err
		}
	}
	if key := rawKey(node, "entries", "Entries"); key != "" {
		var entries []map[string]json.RawMessage
		if err := json.Unmarshal(node[key], &entries); err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if basedKey := rawKey(entry, "based_on", "BasedOn"); basedKey != "" {
				if err := remapRawStringSlice(entry, basedKey, mapper.entryRef); err != nil {
					return nil, err
				}
			}
			if disputesKey := rawKey(entry, "disputes", "Disputes"); disputesKey != "" {
				if err := remapRawStringSlice(entry, disputesKey, mapper.entryRef); err != nil {
					return nil, err
				}
			}
		}
		node[key], _ = json.Marshal(entries)
	}
	return json.Marshal(node)
}

func remapRawString(obj map[string]json.RawMessage, key string, f func(string) (string, error)) error {
	raw := obj[key]
	if len(raw) == 0 {
		return nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	mapped, err := f(value)
	if err != nil {
		return err
	}
	obj[key], _ = json.Marshal(mapped)
	return nil
}

func remapRawStringSlice(obj map[string]json.RawMessage, key string, f func(string) (string, error)) error {
	raw := obj[key]
	if len(raw) == 0 {
		return nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return err
	}
	for i := range values {
		mapped, err := f(values[i])
		if err != nil {
			return err
		}
		values[i] = mapped
	}
	obj[key], _ = json.Marshal(values)
	return nil
}

func transformFlowJSONL(data []byte, mapper importPathMapper) ([]byte, error) {
	var out bytes.Buffer
	for lineNo, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var root map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &root); err != nil {
			return nil, fmt.Errorf("flow JSONL 第 %d 行:%w", lineNo+1, err)
		}
		flowObj := root
		wrapped := false
		if raw := root["flow"]; len(raw) > 0 {
			flowObj = map[string]json.RawMessage{}
			if err := json.Unmarshal(raw, &flowObj); err != nil {
				return nil, err
			}
			wrapped = true
		}
		if raw := flowObj["steps"]; len(raw) > 0 {
			var steps []map[string]json.RawMessage
			if err := json.Unmarshal(raw, &steps); err != nil {
				return nil, err
			}
			for _, step := range steps {
				if err := remapRawString(step, "node", mapper.nodeID); err != nil {
					return nil, err
				}
			}
			flowObj["steps"], _ = json.Marshal(steps)
		}
		if wrapped {
			root["flow"], _ = json.Marshal(flowObj)
		}
		encoded, err := json.Marshal(root)
		if err != nil {
			return nil, err
		}
		out.Write(encoded)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// validateImportStage 在写盘前构造导入后的最终视图，校验 Node/Flow ID、文件归属
// 及 bundle 新增引用。被覆盖文件先从旧视图移除，因此合法的同路径替换不误报。
func (e *Engine) validateImportStage(staged []stagedImportFile) error {
	replaced := map[string]bool{}
	for _, file := range staged {
		replaced[file.name] = true
	}
	if err := e.validateFinalJournals(staged, replaced); err != nil {
		return err
	}
	if !replaced["config.yaml"] {
		if _, err := e.Store.LoadConfig(); err != nil {
			return fmt.Errorf("验证现有 config.yaml:%w", err)
		}
	}
	nodes := map[string]model.Node{}
	nodeOrigin := map[string]string{}
	type refCheck struct {
		owner string
		ref   string
	}
	var finalEntryRefs []refCheck
	type flowCheck struct {
		flow   model.Flow
		origin string
	}
	var stagedFlows []flowCheck
	addNode := func(n model.Node, origin string) error {
		if err := validateImportedNode(n); err != nil {
			return fmt.Errorf("%s: %w", origin, err)
		}
		if !model.SafeNodeID(n.ID) {
			return fmt.Errorf("%s 含非法 Node ID %q", origin, n.ID)
		}
		if prev := nodeOrigin[n.ID]; prev != "" {
			return fmt.Errorf("导入后 Node ID 冲突:%s 同时出现在 %s 与 %s", n.ID, prev, origin)
		}
		nodes[n.ID], nodeOrigin[n.ID] = n, origin
		for _, entry := range n.Entries {
			for _, ref := range append(append([]string(nil), entry.BasedOn...), entry.Disputes...) {
				finalEntryRefs = append(finalEntryRefs, refCheck{owner: n.ID + "#" + entry.ID, ref: ref})
			}
		}
		return nil
	}

	for _, file := range staged {
		switch {
		case file.name == "project.yaml":
			var sh store.Shard
			if err := yaml.Unmarshal(file.data, &sh); err != nil {
				return fmt.Errorf("解析 staging %s: %w", file.name, err)
			}
			if len(sh.Nodes) != 1 || sh.Nodes[0].ID != model.ProjectNodeID {
				return fmt.Errorf("staging project.yaml 必须且只能包含项目节点 %q", model.ProjectNodeID)
			}
			if err := addNode(sh.Nodes[0], file.name); err != nil {
				return err
			}
		case strings.HasPrefix(file.name, "tree/"):
			var sh store.Shard
			if err := yaml.Unmarshal(file.data, &sh); err != nil {
				return fmt.Errorf("解析 staging %s: %w", file.name, err)
			}
			expected := strings.TrimSuffix(strings.TrimPrefix(file.name, "tree/"), ".yaml")
			if strings.HasSuffix(expected, "/_dir") {
				expected = strings.TrimSuffix(expected, "/_dir") + "/"
			}
			for _, n := range sh.Nodes {
				nodeFile, _ := model.SplitNodeID(n.ID)
				if nodeFile != expected {
					return fmt.Errorf("staging 分片路径/Node ID 错位:%s 包含 %s", file.name, n.ID)
				}
				if err := addNode(n, file.name); err != nil {
					return err
				}
			}
		case strings.HasPrefix(file.name, "flows/"), strings.HasPrefix(file.name, "topics/"):
			flows, err := decodeStagedFlows(file)
			if err != nil {
				return err
			}
			for _, flow := range flows {
				stagedFlows = append(stagedFlows, flowCheck{flow: flow, origin: file.name})
			}
		}
	}

	// project.yaml 参与 bundle 替换；未被 staging 替换时加入最终引用空间。
	if !replaced["project.yaml"] {
		if sh, _, err := e.Store.LoadShard(e.Store.ProjectShardPath()); err == nil {
			for _, n := range sh.Nodes {
				if err := addNode(n, "project.yaml"); err != nil {
					return err
				}
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("验证 project.yaml: %w", err)
		}
	}

	treeDir := filepath.Join(e.Store.Dir(), "tree")
	if err := filepath.WalkDir(treeDir, func(filePath string, d os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".yaml") {
			return nil
		}
		rel, err := filepath.Rel(e.Store.Dir(), filePath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if replaced[rel] {
			return nil
		}
		sh, _, err := e.Store.LoadShard(filePath)
		if err != nil {
			return fmt.Errorf("验证现有分片 %s: %w", rel, err)
		}
		for _, n := range sh.Nodes {
			if err := addNode(n, rel); err != nil {
				return err
			}
		}
		return nil
	}); err != nil && !os.IsNotExist(err) {
		return err
	}

	entries := map[string]bool{}
	lineage := map[string][]string{}
	for id, n := range nodes {
		for _, entry := range n.Entries {
			entries[id+"#"+entry.ID] = true
		}
		for _, old := range n.Lineage {
			lineage[old] = append(lineage[old], id)
		}
	}
	entryExists := func(ref string) bool {
		if entries[ref] {
			return true
		}
		i := strings.LastIndexByte(ref, '#')
		if i <= 0 {
			return false
		}
		for _, current := range lineage[ref[:i]] {
			if entries[current+ref[i:]] {
				return true
			}
		}
		return false
	}
	for _, check := range finalEntryRefs {
		if !entryExists(check.ref) {
			return fmt.Errorf("staging 引用悬空:%s 引用 %s", check.owner, check.ref)
		}
	}

	flowOrigins := map[string]string{}
	finalFlows := make([]flowCheck, 0, len(stagedFlows))
	addFlow := func(item flowCheck) error {
		if item.flow.ID == "" {
			return fmt.Errorf("%s flow ID 为空", item.origin)
		}
		if prev := flowOrigins[item.flow.ID]; prev != "" {
			return fmt.Errorf("导入后 Flow ID 冲突:%s 同时出现在 %s 与 %s", item.flow.ID, prev, item.origin)
		}
		if strings.HasSuffix(item.origin, ".yaml") {
			wantAbs := e.Store.FlowPathFor(item.flow.ID)
			wantRel, err := filepath.Rel(e.Store.Dir(), wantAbs)
			if wantAbs == "" || err != nil || filepath.ToSlash(wantRel) != item.origin {
				return fmt.Errorf("Flow ID/文件错位:%s 位于 %s", item.flow.ID, item.origin)
			}
		}
		flowOrigins[item.flow.ID] = item.origin
		finalFlows = append(finalFlows, item)
		return nil
	}
	// 先加入 staging flows，并校验 ID 与 yaml 文件路径一致。
	for _, item := range stagedFlows {
		if err := addFlow(item); err != nil {
			return err
		}
	}
	existingFlows, _, err := e.Store.LoadFlows()
	if err != nil {
		return err
	}
	for _, flow := range existingFlows {
		abs := e.Store.FlowPathFor(flow.ID)
		rel, relErr := filepath.Rel(e.Store.Dir(), abs)
		if abs == "" || relErr != nil {
			continue // LoadFlows 已把非法 ID/文件名隔离；此处保持同一可见性语义。
		}
		rel = filepath.ToSlash(rel)
		if replaced[rel] {
			continue
		}
		if err := addFlow(flowCheck{flow: flow, origin: rel}); err != nil {
			return err
		}
	}
	for _, item := range finalFlows {
		for _, step := range item.flow.Steps {
			if _, ok := nodes[step.Node]; !ok && len(lineage[step.Node]) == 0 {
				return fmt.Errorf("最终 flow %s(%s) 引用不存在节点 %s", item.flow.ID, item.origin, step.Node)
			}
		}
	}
	return nil
}

func validateImportedNode(n model.Node) error {
	if n.ID == "" || !utf8.ValidString(n.ID) || !model.SafeNodeID(n.ID) {
		return fmt.Errorf("非法 Node ID %q", n.ID)
	}
	file, symbol := model.SplitNodeID(n.ID)
	if strings.Contains(symbol, "#") || strings.ContainsAny(symbol, "\r\n\x00") || n.Anchor.File != file || n.Anchor.Symbol != symbol {
		return fmt.Errorf("Node %s 的 Anchor.File/Symbol=(%q,%q) 与 ID 不一致", n.ID, n.Anchor.File, n.Anchor.Symbol)
	}
	switch n.Level {
	case model.LevelProject:
		if n.ID != model.ProjectNodeID {
			return fmt.Errorf("project level 节点 ID 必须是 %q", model.ProjectNodeID)
		}
	case model.LevelDir:
		if !strings.HasSuffix(n.ID, "/") || symbol != "" {
			return fmt.Errorf("dir level 节点 ID 必须以 / 结尾")
		}
	case model.LevelFile:
		if symbol != "" || strings.HasSuffix(n.ID, "/") || n.ID == model.ProjectNodeID {
			return fmt.Errorf("file level 节点 ID 不能含 symbol")
		}
	case model.LevelFunction, model.LevelDecl, model.LevelStmt:
		if symbol == "" || file == model.ProjectNodeID || strings.HasSuffix(file, "/") {
			return fmt.Errorf("%s level 节点必须含非空 symbol", n.Level)
		}
	default:
		return fmt.Errorf("Node %s level %q 非法", n.ID, n.Level)
	}
	if !validImportedStatus(n.Status) {
		return fmt.Errorf("Node %s status %q 非法", n.ID, n.Status)
	}
	seen := map[string]bool{}
	for _, entry := range n.Entries {
		if !safeImportedEntryID(entry.ID) {
			return fmt.Errorf("Node %s 含不安全 Entry ID %q", n.ID, entry.ID)
		}
		if seen[entry.ID] {
			return fmt.Errorf("Node %s 含重复 Entry ID %s", n.ID, entry.ID)
		}
		if !validImportedKind(entry.Kind) {
			return fmt.Errorf("Node %s Entry %s kind %q 非法", n.ID, entry.ID, entry.Kind)
		}
		if !validImportedConfidence(entry.Confidence) {
			return fmt.Errorf("Node %s Entry %s confidence %q 非法", n.ID, entry.ID, entry.Confidence)
		}
		seen[entry.ID] = true
	}
	return nil
}

func validImportedStatus(status model.Status) bool {
	switch status {
	case model.StatusFresh, model.StatusSuspect, model.StatusOrphaned, model.StatusUndigested:
		return true
	default:
		return false
	}
}

func validImportedKind(kind string) bool {
	switch kind {
	case model.KindSummary, model.KindContract, model.KindMutation, model.KindPitfall, model.KindUsage:
		return true
	default:
		return false
	}
}

func validImportedConfidence(confidence model.Confidence) bool {
	switch confidence {
	case model.ConfidenceDerived, model.ConfidenceVerified, model.ConfidenceInferred,
		model.ConfidenceSuspect, model.ConfidenceRefuted:
		return true
	default:
		return false
	}
}

func safeImportedEntryID(id string) bool {
	if id == "" || len(id) > 256 || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) || unicode.IsSpace(r) || r == '#' || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}

func (e *Engine) validateFinalJournals(staged []stagedImportFile, replaced map[string]bool) error {
	byID := map[string]string{} // ID -> canonical JSON；字段顺序/空白不影响一致性
	byOrigin := map[string]string{}
	check := func(data []byte, origin string, strict bool) error {
		month := strings.TrimSuffix(path.Base(origin), ".jsonl")
		for lineNo, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			canonical, id, err := canonicalJournalLine(line)
			if err != nil || id == "" {
				if strict {
					if err == nil {
						err = fmt.Errorf("change ID 为空")
					}
					return fmt.Errorf("%s 第 %d 行:%w", origin, lineNo+1, err)
				}
				continue // 未替换的旧坏行仍遵守 store 读端容错契约
			}
			if !safeImportedEntryID(id) {
				return fmt.Errorf("%s 第 %d 行 change ID %q 不安全", origin, lineNo+1, id)
			}
			var change model.Change
			if err := json.Unmarshal([]byte(line), &change); err != nil || change.At.IsZero() {
				return fmt.Errorf("%s 第 %d 行 change.At 缺失或非法", origin, lineNo+1)
			}
			if change.EffectsVersion > 1 {
				return fmt.Errorf("%s 第 %d 行 change %s effects_version=%d 高于当前引擎", origin, lineNo+1, id, change.EffectsVersion)
			}
			if err := validateImportedChange(change); err != nil {
				return fmt.Errorf("%s 第 %d 行 change %s:%w", origin, lineNo+1, id, err)
			}
			if want := change.At.UTC().Format("2006-01"); month != want {
				return fmt.Errorf("%s 第 %d 行 change %s 的 At 归属 %s，不得放在 %s", origin, lineNo+1, id, want, month)
			}
			if prev := byID[id]; prev != "" && prev != canonical {
				return fmt.Errorf("导入后 change ID 冲突:%s 在 %s 与 %s 第 %d 行内容不同", id, byOrigin[id], origin, lineNo+1)
			}
			byID[id], byOrigin[id] = canonical, origin
		}
		return nil
	}
	for _, file := range staged {
		if strings.HasPrefix(file.name, "journal/") {
			if err := check(file.data, file.name, true); err != nil {
				return err
			}
		}
	}
	dir := filepath.Join(e.Store.Dir(), "journal")
	if err := filepath.WalkDir(dir, func(filePath string, d os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		rel, err := filepath.Rel(e.Store.Dir(), filePath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if replaced[rel] {
			return nil
		}
		data, err := e.Store.ReadKnowledgeFile(rel)
		if err != nil {
			return err
		}
		return check(data, rel, false)
	}); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func validateImportedChange(change model.Change) error {
	for _, node := range change.Nodes {
		if !model.SafeNodeID(node) {
			return fmt.Errorf("含非法 node %q", node)
		}
	}
	for _, effect := range change.Effects {
		i := strings.LastIndexByte(effect.Entry, '#')
		if i <= 0 || !model.SafeNodeID(effect.Entry[:i]) || !safeImportedEntryID(effect.Entry[i+1:]) {
			return fmt.Errorf("含非法 entry effect 引用 %q", effect.Entry)
		}
		if !validImportedConfidence(effect.Before.Confidence) || !validImportedConfidence(effect.After.Confidence) {
			return fmt.Errorf("entry effect %s 含非法 confidence before=%q after=%q", effect.Entry,
				effect.Before.Confidence, effect.After.Confidence)
		}
	}
	for _, effect := range change.NodeEffects {
		if !model.SafeNodeID(effect.Node) {
			return fmt.Errorf("含非法 node effect ID %q", effect.Node)
		}
		for label, node := range map[string]*model.Node{"before": effect.Before, "after": effect.After} {
			if node == nil {
				continue
			}
			if node.ID != effect.Node {
				return fmt.Errorf("node effect %s 快照 ID %q != effect.Node %q", label, node.ID, effect.Node)
			}
			if err := validateImportedNode(*node); err != nil {
				return fmt.Errorf("node effect %s:%w", label, err)
			}
		}
		for label, shard := range map[string]string{"before_shard": effect.BeforeShard, "after_shard": effect.AfterShard} {
			if shard == "" {
				continue
			}
			if _, ok := importableBundleEntry(shard); !ok || !strings.HasPrefix(shard, "tree/") {
				return fmt.Errorf("node effect %s %q 非法", label, shard)
			}
		}
	}
	for _, remap := range change.Remaps {
		if !model.SafeNodeID(remap.From) {
			return fmt.Errorf("含非法 remap.from %q", remap.From)
		}
		for _, target := range remap.To {
			if !model.SafeNodeID(target) {
				return fmt.Errorf("含非法 remap.to %q", target)
			}
		}
		for entryID, target := range remap.Entries {
			if !safeImportedEntryID(entryID) || !model.SafeNodeID(target) {
				return fmt.Errorf("含非法 remap.entries %q=%q", entryID, target)
			}
		}
	}
	return nil
}

func decodeStagedFlows(file stagedImportFile) ([]model.Flow, error) {
	if !strings.HasSuffix(file.name, ".yaml") {
		return nil, fmt.Errorf("flow/topic 只支持顶层 YAML:%s", file.name)
	}
	var shard model.FlowShard
	if err := yaml.Unmarshal(file.data, &shard); err != nil || shard.Flow.ID == "" {
		return nil, fmt.Errorf("解析 flow %s 失败", file.name)
	}
	return []model.Flow{shard.Flow}, nil
}
