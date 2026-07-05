package parser

import (
	"strings"
	"testing"
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
	for _, want := range []string{"top(func)", "fetch(func)", "Service(class)",
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
}

func TestPythonSyntaxError(t *testing.T) {
	if !PythonAvailable() {
		t.Skip("无 python3")
	}
	if _, err := (Python{}).Parse("a.py", []byte("def broken(:\n")); err == nil {
		t.Error("语法错误应报 error(解析失败三态)")
	}
}
