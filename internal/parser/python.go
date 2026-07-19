package parser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Python 解析器(2026-07-04 多语言 T1,自托管范式):让语言用**自己的工具链**
// 解析自己——python3 内置 ast 模块经助手脚本吐 JSON,Go 侧零新依赖(铁律一);
// 运行时依赖仅在仓库真有 .py 时才需要(python3 不在 PATH → 不注册,.py 不索引,
// 可用 config extensions 白名单降级为文件级覆盖)。
//
// 粒度与哈希语义对齐 Go 插件:
//   - 符号 = 顶层 def/async def/class + 类方法(Class.method 规范名;嵌套函数属外层 body);
//   - Hash = sha256("py\0"+kind+"\0"+ast.dump(节点)):格式化/缩进/# 注释免疫
//     (# 注释不在 AST——注释变更不失配,弱于 Go 的 doc 参与,留痕;docstring 在
//     AST 内,变更失配,与 Go 的 doc 语义等效);
//   - StructHash = 自身名换占位符后的 dump:改名免疫,仅迁移匹配;
//   - class 符号的哈希剥离方法体(方法另有符号)——方法改动不连坐 class 节点。
//
// 成本留痕:每文件一次 python3 子进程(~30-50ms);init 全库扫描在纯 Python
// 大仓上分钟级,增量路径(remember/recall 单文件)无感。调用图暂不提供
// (CallExtractor 需要 Python 自己的 import 归位规则,等真实使用信号)。
type Python struct{}

func (Python) Language() string { return "python" }

func (Python) Extensions() []string { return []string{".py"} }

// pythonExe 探测一次(python3 优先,windows 常为 python;探测含真实执行防
// Microsoft Store 假 stub)。空串 = 不可用。5s 超时护栏:坏 python(网络盘
// stdlib、劫持的 sitecustomize)挂死时不许拖垮 serve 启动——纯 Go 仓库不陪葬。
var pythonExe = sync.OnceValue(func() string {
	for _, name := range []string{"python3", "python"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := runPythonProbe(ctx, p, safePythonDir(p))
		cancel()
		if err == nil && bytes.HasPrefix(bytes.TrimSpace(out), []byte("1")) {
			return p
		}
	}
	return ""
})

func safePythonDir(exe string) string {
	abs, err := filepath.Abs(exe)
	if err == nil {
		exe = abs
	}
	return filepath.Dir(exe)
}

// minimalPythonEnv 清除 PYTHONPATH/PYTHONHOME/PYTHONSTARTUP 等注入面。
// Windows 保留进程启动与临时目录所需的系统变量；解释器路径已由 LookPath
// 固定，后续 helper 不依赖 PATH 查找任何子程序。
func minimalPythonEnv() []string {
	keys := []string{"PATH"}
	if runtime.GOOS == "windows" {
		keys = append(keys, "SYSTEMROOT", "WINDIR", "TEMP", "TMP")
	}
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func isolatedPythonCommand(ctx context.Context, exe, dir, script string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, exe, "-I", "-S", "-c", script)
	cmd.Dir = dir
	cmd.Env = minimalPythonEnv()
	return cmd
}

func runPythonProbe(ctx context.Context, exe, dir string) ([]byte, error) {
	return isolatedPythonCommand(ctx, exe, dir, "import ast,json,hashlib;print(1)").Output()
}

// PythonAvailable 报告本机可否解析 Python(Registry 注册前置)。
func PythonAvailable() bool { return pythonExe() != "" }

// pySymbol 是助手脚本的输出行。
type pySymbol struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
	Line1      int    `json:"line1"`
	Line2      int    `json:"line2"`
	Hash       string `json:"hash"`
	StructHash string `json:"struct_hash"`
}

type pyResult struct {
	Symbols  []pySymbol `json:"symbols"`
	FileHash string     `json:"file_hash"`
}

// Python Parse 与紧随其后的 HashFileFor 共用同一次 AST 助手结果。
// 小容量有界缓存避免大仓每文件额外再启一个 python 进程。
var pyFileHashes = struct {
	sync.Mutex
	values map[[sha256.Size]byte]string
	order  [][sha256.Size]byte
}{values: make(map[[sha256.Size]byte]string)}

const pyFileHashCacheLimit = 64

func cachePythonFileHash(src []byte, fileHash string) {
	key := sha256.Sum256(src)
	pyFileHashes.Lock()
	defer pyFileHashes.Unlock()
	if _, exists := pyFileHashes.values[key]; exists {
		pyFileHashes.values[key] = fileHash
		return
	}
	if len(pyFileHashes.values) >= pyFileHashCacheLimit {
		oldest := pyFileHashes.order[0]
		pyFileHashes.order = pyFileHashes.order[1:]
		delete(pyFileHashes.values, oldest)
	}
	pyFileHashes.values[key] = fileHash
	pyFileHashes.order = append(pyFileHashes.order, key)
}

func takePythonFileHash(src []byte) (string, bool) {
	key := sha256.Sum256(src)
	pyFileHashes.Lock()
	defer pyFileHashes.Unlock()
	fileHash, ok := pyFileHashes.values[key]
	if !ok {
		return "", false
	}
	delete(pyFileHashes.values, key)
	for i := range pyFileHashes.order {
		if pyFileHashes.order[i] == key {
			pyFileHashes.order = append(pyFileHashes.order[:i], pyFileHashes.order[i+1:]...)
			break
		}
	}
	return fileHash, true
}

// HashFile 以整棵 Python AST 为锚，不再只级联函数/类符号。
// 因此 import、模块常量和顶层执行语句的语义变更都会使文件腐烂。
func (Python) HashFile(src []byte) string {
	if fileHash, ok := takePythonFileHash(src); ok {
		return "sha256:" + fileHash
	}
	if result, err := runPythonHelper(src); err == nil && result.FileHash != "" {
		return "sha256:" + result.FileHash
	}
	// 理论上 Parse 失败时不会进入此处；若运行时在两步之间失效，
	// 则用原文 fail closed，宁可多报腐烂也不可对变更失明。
	h := sha256.Sum256(append([]byte("py\x00file-raw\x00"), src...))
	return "sha256:" + hex.EncodeToString(h[:])
}

// Parse 经助手进程提取符号(src 走 stdin,JSON 走 stdout)。
func (Python) Parse(path string, src []byte) ([]Symbol, error) {
	return (Python{}).ParseContext(context.Background(), path, src)
}

func (Python) ParseContext(ctx context.Context, path string, src []byte) ([]Symbol, error) {
	raw, err := runPythonHelperContext(ctx, src)
	if err != nil {
		return nil, err
	}
	cachePythonFileHash(src, raw.FileHash)
	syms := make([]Symbol, 0, len(raw.Symbols))
	for i, r := range raw.Symbols {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		start, end := r.Start, min(r.End, len(src))
		if start < 0 || start > end {
			continue // 偏移异常的符号丢弃,宁缺
		}
		syms = append(syms, Symbol{
			Name: r.Name, Kind: r.Kind,
			Start: start, End: end,
			Body:          src[start:end],
			Lines:         [2]int{r.Line1, r.Line2},
			Hash:          "sha256:" + r.Hash,
			StructHash:    "sha256:" + r.StructHash,
			DocStructHash: "sha256:" + r.StructHash,
		})
	}
	disambiguate(syms) // ~n 消歧与 Go 同规则
	return syms, nil
}

func runPythonHelper(src []byte) (pyResult, error) {
	return runPythonHelperContext(context.Background(), src)
}

func runPythonHelperContext(parent context.Context, src []byte) (pyResult, error) {
	if parent == nil {
		return pyResult{}, fmt.Errorf("python 解析: nil context")
	}
	if err := parent.Err(); err != nil {
		return pyResult{}, err
	}
	exe := pythonExe()
	if exe == "" {
		return pyResult{}, fmt.Errorf("python3 不可用(未注册时不应到达)")
	}
	// 20s 超时:单文件解析挂死不许阻塞整库(engine 多数调用点持锁)。
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	cmd := isolatedPythonCommand(ctx, exe, safePythonDir(exe), pyHelper)
	cmd.Stdin = bytes.NewReader(src)
	// -I -S + 最小环境 + 非仓库 cwd：不加载 site/user-site，也不把当前仓库
	// 放进 sys.path。ast.parse 不执行源码；编码查找也只经解释器 stdlib。
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	if err := cmd.Run(); err != nil {
		if err := parent.Err(); err != nil {
			return pyResult{}, err
		}
		if ctx.Err() != nil {
			return pyResult{}, fmt.Errorf("python 解析超时(20s 护栏)")
		}
		// SyntaxError 等 → 解析失败三态(init 跳过计数/remember 拒收/记账照收)。
		return pyResult{}, fmt.Errorf("python 解析失败:%s", firstLineOf(errBuf.String()))
	}
	var raw pyResult
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		return pyResult{}, fmt.Errorf("python 助手输出不可解:%w", err)
	}
	if raw.FileHash == "" {
		return pyResult{}, fmt.Errorf("python 助手缺文件哈希")
	}
	return raw, nil
}

func firstLineOf(s string) string {
	if i := bytes.IndexByte([]byte(s), '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// pyHelper 是嵌入的助手脚本(仅 python3 stdlib)。
const pyHelper = `
import ast, sys, json, hashlib, copy, io, tokenize

src = sys.stdin.buffer.read()
encoding, _ = tokenize.detect_encoding(io.BytesIO(src).readline)
text = src.decode(encoding, "strict")
tree = ast.parse(text)
file_hash = hashlib.sha256(("py\x00file\x00" + ast.dump(tree)).encode()).hexdigest()

# AST 的 col_offset 是“解码文本重新编码为 UTF-8”后的字节列；Go Body 切片
# 必须落回原文件字节（PEP 263 可不是 UTF-8）。先按原始行累积，再把 UTF-8
# 列解成字符前缀，按源编码重编码取得原始字节列。
lines = text.splitlines(keepends=True)
raw_lines = src.splitlines(keepends=True)
if not lines:
    lines = [""]
if not raw_lines:
    raw_lines = [b""]
offs = [0]
for raw_line in raw_lines:
    offs.append(offs[-1] + len(raw_line))

def bpos(lineno, col):
    line = lines[lineno - 1]
    prefix = line.encode("utf-8")[:col].decode("utf-8", "strict")
    enc = "utf-8" if encoding.lower().replace("_", "-") == "utf-8-sig" and lineno > 1 else encoding
    return offs[lineno - 1] + len(prefix.encode(enc))

def span(node):
    l1 = node.lineno
    if getattr(node, "decorator_list", None):
        l1 = min(d.lineno for d in node.decorator_list)
    start = bpos(l1, 0)
    end = bpos(node.end_lineno, node.end_col_offset)
    return start, end, l1, node.end_lineno

def hashes(kind, node, strip_methods=False):
    n = node
    if strip_methods:
        n = copy.deepcopy(node)
        body = []
        for x in n.body:
            if isinstance(x, (ast.FunctionDef, ast.AsyncFunctionDef)):
                x.body = [ast.Pass()]
            body.append(x)
        n.body = body or [ast.Pass()]
    dumped = ast.dump(n)
    h = hashlib.sha256(("py\x00" + kind + "\x00" + dumped).encode()).hexdigest()
    saved = n.name
    n.name = "_$SELF$_"
    dumped2 = ast.dump(n)
    n.name = saved
    sh = hashlib.sha256(("py\x00" + kind + "\x00" + dumped2).encode()).hexdigest()
    return h, sh

out = []
def emit(name, kind, node, strip_methods=False):
    start, end, l1, l2 = span(node)
    h, sh = hashes(kind, node, strip_methods)
    out.append({"name": name, "kind": kind, "start": start, "end": end,
                "line1": l1, "line2": l2, "hash": h, "struct_hash": sh})

for node in tree.body:
    if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
        emit(node.name, "func", node)
    elif isinstance(node, ast.ClassDef):
        # class 符号剥方法体(方法另有符号,改方法不连坐 class);方法逐个入符号。
        emit(node.name, "type", node, strip_methods=True)
        for m in node.body:
            if isinstance(m, (ast.FunctionDef, ast.AsyncFunctionDef)):
                emit(node.name + "." + m.name, "method", m)

json.dump({"symbols": out, "file_hash": file_hash}, sys.stdout)
`
