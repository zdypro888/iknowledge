// Package parser 定义解析器插件接口与 Go 实现(impl §5)。
// 核心引擎与语言无关,语言解析是插件:输入源文件,输出符号列表 + 代码单元边界与双哈希。
package parser

import (
	"bytes"
	"path"
	"regexp"
	"strings"
)

// Symbol 是从源文件提取出的一个符号(impl §5 定稿)。
type Symbol struct {
	Name  string // 规范名,文法见 impl §3(接收者去指针去类型参数;同名符号带 ~n 序号)
	Kind  string // func | method | type | var | const
	Start int    // 字节偏移,含 doc comment
	End   int
	Body  []byte // [Start:End) 原文
	Lines [2]int
	// 双哈希在 Parse 时计算(依赖 AST,离开 parser 无从复算):
	Hash       string // 锚定/腐烂检测:go/printer 标准重打印(gofmt 免疫),含 doc comment
	StructHash string // 迁移匹配:剥全部注释、自身标识符换占位符;绝不用于腐烂检测
}

// Parser 是解析器插件接口(impl §5)。
type Parser interface {
	Language() string     // "go"
	Extensions() []string // [".go"]
	Parse(path string, src []byte) ([]Symbol, error)
}

// FileHasher 是插件的可选能力:自定义文件级锚定哈希(2026-07-04 多语言修订)。
// 缺省(不实现)用 FileHash(syms)=符号哈希级联——依赖真 AST 的格式化免疫;
// 无符号提取的插件(Generic)必须实现它,否则空符号级联出常量哈希,腐烂检测失明。
type FileHasher interface {
	HashFile(src []byte) string
}

// HashFileFor 统一出口:插件自定义优先,否则符号级联(engine 各锚定点共用)。
func HashFileFor(p Parser, syms []Symbol, src []byte) string {
	if fh, ok := p.(FileHasher); ok {
		return fh.HashFile(src)
	}
	return FileHash(syms)
}

// Registry 按扩展名分发解析器。
type Registry struct {
	byExt map[string]Parser
}

// NewRegistry 返回注册了全部内置解析器的注册表:Go(go/ast)恒在;
// Python(自托管助手,2026-07-04 多语言 T1)按本机 python3 可用性注册——
// 不可用则 .py 不索引(可经 config extensions 白名单降级为文件级覆盖)。
// TypeScript/JavaScript(R29 批次6,纯 Go 词法,零运行时依赖)恒注册。
func NewRegistry() *Registry {
	r := &Registry{byExt: map[string]Parser{}}
	r.Register(Golang{})
	if PythonAvailable() {
		r.Register(Python{})
	}
	r.Register(TypeScript{}) // 纯 Go 词法,无需运行时探测
	r.Register(Rust{})
	r.Register(Java{})
	return r
}

// Register 注册一个解析器插件。
func (r *Registry) Register(p Parser) {
	for _, ext := range p.Extensions() {
		r.byExt[ext] = p
	}
}

// ForFile 返回能解析该文件的插件;没有则返回 nil。
func (r *Registry) ForFile(file string) Parser {
	return r.byExt[path.Ext(file)]
}

// generatedRe 是 Go 官方生成代码约定(impl §5 排除策略)。
var generatedRe = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

// IsGenerated 判断源码是否为生成代码:首行匹配官方约定(impl §5 定案按首行)。
// 全程 []byte 操作:原实现两次 string(src) 整文件拷贝,init 全库扫描是热路径。
func IsGenerated(src []byte) bool {
	line, _, _ := bytes.Cut(src, []byte("\n")) // 未含换行时 line 即整个 src
	return generatedRe.Match(bytes.TrimSuffix(line, []byte("\r")))
}

// ExcludedPath 判断 repo 相对路径(正斜杠)是否落在默认排除段内:
// vendor/、testdata/、.knowledge/(impl §5)。任意一段命中即排除。
func ExcludedPath(rel string) bool {
	for seg := range strings.SplitSeq(rel, "/") {
		switch seg {
		case "vendor", "testdata", ".knowledge":
			return true
		}
	}
	return false
}
