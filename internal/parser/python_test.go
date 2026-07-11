package parser

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const pySrc = `import os

@deco
def top(a, b):
    """docstring here"""
    return a + b

async def fetch(url):
    return url

class Service:
    """service doc"""

    LIMIT = 3

    def start(self):
        return 1

    async def stop(self):
        pass
`

func TestPythonProbeDoesNotExecuteRepositorySitecustomize(t *testing.T) {
	var exe string
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			exe = p
			break
		}
	}
	if exe == "" {
		t.Skip("无 python3")
	}
	untrusted := t.TempDir()
	marker := filepath.Join(untrusted, "probe-executed")
	payload := fmt.Sprintf("open(%q, 'w').write('owned')\n", marker)
	if err := os.WriteFile(filepath.Join(untrusted, "sitecustomize.py"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := runPythonProbe(ctx, exe, untrusted)
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(out)), "1") {
		t.Fatalf("隔离 probe 失败: out=%q err=%v", out, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("Python probe 执行了仓库 sitecustomize: %v", err)
	}
}

func TestPythonParse(t *testing.T) {
	if !PythonAvailable() {
		t.Skip("无 python3")
	}
	syms, err := (Python{}).Parse("a.py", []byte(pySrc))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	var names []string
	for _, s := range syms {
		got[s.Name] = s
		names = append(names, s.Name+"("+s.Kind+")")
	}
	for _, want := range []string{"top(func)", "fetch(func)", "Service(type)",
		"Service.start(method)", "Service.stop(method)"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("缺符号 %s(实得 %v)", want, names)
		}
	}
	// 装饰器计入符号范围;Body 是真实源码切片。
	if top := got["top"]; !strings.HasPrefix(string(top.Body), "@deco") || top.Lines[0] != 3 {
		t.Errorf("top 范围/行号不对:lines=%v body=%q", top.Lines, string(top.Body)[:20])
	}
}

func TestPythonFileHashCoversModuleSemantics(t *testing.T) {
	if !PythonAvailable() {
		t.Skip("无 python3")
	}
	hash := func(src string) string {
		t.Helper()
		syms, err := (Python{}).Parse("module.py", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		return HashFileFor(Python{}, syms, []byte(src))
	}
	base := "import os\nLIMIT = 3\nprint(LIMIT)\n"
	tests := []struct {
		name string
		src  string
		same bool
	}{
		{name: "注释与空行免疫", src: "# note\nimport os\n\nLIMIT=3\nprint(LIMIT)\n", same: true},
		{name: "import 路径变更", src: "import sys\nLIMIT = 3\nprint(LIMIT)\n"},
		{name: "模块常量变更", src: "import os\nLIMIT = 4\nprint(LIMIT)\n"},
		{name: "顶层执行语句变更", src: "import os\nLIMIT = 3\nlog(LIMIT)\n"},
	}
	baseHash := hash(base)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hash(tt.src) == baseHash; got != tt.same {
				t.Fatalf("文件哈希相同=%v, want %v", got, tt.same)
			}
		})
	}
}

func TestPythonHashSemantics(t *testing.T) {
	if !PythonAvailable() {
		t.Skip("无 python3")
	}
	parse := func(src string) map[string]Symbol {
		syms, err := (Python{}).Parse("a.py", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]Symbol{}
		for _, s := range syms {
			m[s.Name] = s
		}
		return m
	}
	base := parse(pySrc)

	// ① 格式化免疫:加空行与 # 注释 → 哈希不变。
	reformatted := strings.Replace(pySrc, "def top(a, b):", "def top(a, b):  # a comment\n", 1)
	reformatted = strings.Replace(reformatted, "class Service:", "\n\nclass Service:", 1)
	re := parse(reformatted)
	if re["top"].Hash != base["top"].Hash {
		t.Error("# 注释/空行应免疫(Hash 变了)")
	}
	// ② 改名:Hash 变,StructHash 不变(迁移匹配的根基)。
	renamed := parse(strings.Replace(pySrc, "def top(", "def summit(", 1))
	if renamed["summit"].Hash == base["top"].Hash {
		t.Error("改名后 Hash 应变化")
	}
	if renamed["summit"].StructHash != base["top"].StructHash {
		t.Error("改名后 StructHash 应稳定(迁移匹配失效)")
	}
	// ③ docstring 变更 → 失配(与 Go 的 doc 参与语义等效)。
	doc := parse(strings.Replace(pySrc, "docstring here", "docstring changed", 1))
	if doc["top"].Hash == base["top"].Hash {
		t.Error("docstring 变更应失配")
	}
	// ④ 方法体变更:方法失配,class 符号不连坐(方法体已剥离)。
	meth := parse(strings.Replace(pySrc, "return 1", "return 2", 1))
	if meth["Service.start"].Hash == base["Service.start"].Hash {
		t.Error("方法体变更应失配")
	}
	if meth["Service"].Hash != base["Service"].Hash {
		t.Error("方法体变更不应连坐 class 符号")
	}
	// ⑤ class 级字段变更 → class 失配。
	cls := parse(strings.Replace(pySrc, "LIMIT = 3", "LIMIT = 5", 1))
	if cls["Service"].Hash == base["Service"].Hash {
		t.Error("class 级语句变更应失配")
	}
	// ⑥ 方法签名属于 class 结构；只剥实现正文，不能把方法整个删除。
	sig := parse(strings.Replace(pySrc, "def start(self):", "def start(self, mode=1):", 1))
	if sig["Service"].Hash == base["Service"].Hash {
		t.Error("方法签名变更应使 class 哈希失配")
	}
	removed := parse(strings.Replace(pySrc, "\n    def start(self):\n        return 1\n", "", 1))
	if removed["Service"].Hash == base["Service"].Hash {
		t.Error("删除方法应使 class 哈希失配")
	}
}

func TestPythonPEP263EncodingAffectsHashesAndBodyOffsets(t *testing.T) {
	if !PythonAvailable() {
		t.Skip("无 python3")
	}
	parse := func(ch byte) (Symbol, string) {
		t.Helper()
		src := append([]byte("# -*- coding: latin-1 -*-\ndef value():\n    return '\""), ch)
		src = append(src, []byte("'\n")...)
		syms, err := (Python{}).Parse("latin.py", src)
		if err != nil || len(syms) != 1 {
			t.Fatalf("合法 PEP 263 文件解析失败: syms=%+v err=%v", syms, err)
		}
		if !strings.Contains(string(syms[0].Body), string([]byte{ch})) {
			t.Fatalf("Body 未按原始字节定位: %x", syms[0].Body)
		}
		return syms[0], HashFileFor(Python{}, syms, src)
	}
	a, fileA := parse(0xe9) // é
	b, fileB := parse(0xea) // ê
	if a.Hash == b.Hash || fileA == fileB {
		t.Fatalf("不同 latin-1 语义被 replacement 解码折叠: symbol %s/%s file %s/%s", a.Hash, b.Hash, fileA, fileB)
	}
	if _, err := (Python{}).Parse("bad.py", []byte("def f():\n    return '\xff'\n")); err == nil {
		t.Fatal("无 encoding cookie 的非法 UTF-8 必须 fail closed")
	}
}

func TestPythonSyntaxError(t *testing.T) {
	if !PythonAvailable() {
		t.Skip("无 python3")
	}
	if _, err := (Python{}).Parse("a.py", []byte("def broken(:\n")); err == nil {
		t.Error("语法错误应报 error(解析失败三态)")
	}
}
