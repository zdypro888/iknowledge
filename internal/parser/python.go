package parser

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
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
// Microsoft Store 假 stub)。空串 = 不可用。
var pythonExe = sync.OnceValue(func() string {
	for _, name := range []string{"python3", "python"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if out, err := exec.Command(p, "-c", "import ast,json,hashlib;print(1)").Output(); err == nil && bytes.HasPrefix(bytes.TrimSpace(out), []byte("1")) {
			return p
		}
	}
	return ""
})

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

// Parse 经助手进程提取符号(src 走 stdin,JSON 走 stdout)。
func (Python) Parse(path string, src []byte) ([]Symbol, error) {
	exe := pythonExe()
	if exe == "" {
		return nil, fmt.Errorf("python3 不可用(未注册时不应到达)")
	}
	cmd := exec.Command(exe, "-c", pyHelper)
	cmd.Stdin = bytes.NewReader(src)
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	if err := cmd.Run(); err != nil {
		// SyntaxError 等 → 解析失败三态(init 跳过计数/remember 拒收/记账照收)。
		return nil, fmt.Errorf("python 解析失败:%s", firstLineOf(errBuf.String()))
	}
	var raw []pySymbol
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("python 助手输出不可解:%w", err)
	}
	syms := make([]Symbol, 0, len(raw))
	for _, r := range raw {
		start, end := r.Start, min(r.End, len(src))
		if start < 0 || start > end {
			continue // 偏移异常的符号丢弃,宁缺
		}
		syms = append(syms, Symbol{
			Name: r.Name, Kind: r.Kind,
			Start: start, End: end,
			Body:       src[start:end],
			Lines:      [2]int{r.Line1, r.Line2},
			Hash:       "sha256:" + r.Hash,
			StructHash: "sha256:" + r.StructHash,
		})
	}
	disambiguate(syms) // ~n 消歧与 Go 同规则
	return syms, nil
}

func firstLineOf(s string) string {
	if i := bytes.IndexByte([]byte(s), '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// pyHelper 是嵌入的助手脚本(仅 python3 stdlib:ast/json/hashlib/sys/copy)。
const pyHelper = `
import ast, sys, json, hashlib, copy

src = sys.stdin.buffer.read()
text = src.decode("utf-8", "replace")
tree = ast.parse(text)

# 行首字节偏移表(UTF-8 字节口径,与 Go 侧 src 切片一致)。
lines = text.split("\n")
offs = [0]
for ln in lines:
    offs.append(offs[-1] + len(ln.encode("utf-8")) + 1)

def bpos(lineno, col):
    return offs[lineno - 1] + len(lines[lineno - 1][:col].encode("utf-8"))

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
        n.body = [x for x in n.body
                  if not isinstance(x, (ast.FunctionDef, ast.AsyncFunctionDef))] or [ast.Pass()]
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
        emit(node.name, "class", node, strip_methods=True)
        for m in node.body:
            if isinstance(m, (ast.FunctionDef, ast.AsyncFunctionDef)):
                emit(node.name + "." + m.name, "method", m)

json.dump(out, sys.stdout)
`
