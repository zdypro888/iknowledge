// Package parser 定义解析器插件接口与 Go 实现(impl §5)。
// 核心引擎与语言无关,语言解析是插件:输入源文件,输出符号列表 + 代码单元边界与双哈希。
package parser

import (
	"bytes"
	"context"
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
	Hash          string // 锚定/腐烂检测:go/printer 标准重打印(gofmt 免疫),含 doc comment
	StructHash    string // 迁移匹配:剥全部注释、自身标识符换占位符;绝不用于腐烂检测
	DocStructHash string // doc 敏感的迁移护栏:仅自身名免疫,契约 doc 改动必失配
}

// Parser 是解析器插件接口(impl §5)。
type Parser interface {
	Language() string     // "go"
	Extensions() []string // [".go"]
	Parse(path string, src []byte) ([]Symbol, error)
}

// ContextParser is an optional parser capability for implementations that may
// block in an external tool. Engine request paths prefer it so MCP
// cancellation can terminate the child process instead of waiting for the
// parser's independent timeout.
type ContextParser interface {
	ParseContext(ctx context.Context, path string, src []byte) ([]Symbol, error)
}

// FileHasher 是插件的可选能力:自定义文件级锚定哈希(2026-07-04 多语言修订)。
// 缺省(不实现)用 FileHash(syms)=符号哈希级联——依赖真 AST 的格式化免疫;
// 无符号提取的插件(Generic)必须实现它,否则空符号级联出常量哈希,腐烂检测失明。
type FileHasher interface {
	HashFile(src []byte) string
}

// ParsedFileHasher 让已有符号结果的插件复用 Parse 成果，避免为文件锚
// 再做一次完整解析。优先级高于只接收原文的 FileHasher。
type ParsedFileHasher interface {
	HashParsedFile(syms []Symbol, src []byte) string
}

// HashFileFor 统一出口:插件自定义优先,否则符号级联(engine 各锚定点共用)。
func HashFileFor(p Parser, syms []Symbol, src []byte) string {
	if fh, ok := p.(ParsedFileHasher); ok {
		return fh.HashParsedFile(syms, src)
	}
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

// IsGenerated 判断源码是否为生成代码。Go 的官方约定允许标记出现在
// “首个非注释/空白文本”之前，不要求它恰好是第一行。一些反向生成的
// protobuf 会先保留来源说明，再写 Code generated 标记；只看首行会把
// 整份 pb.go 误当手写代码索引。这里只扫描最多 64 KiB 前导注释，遇到代码
// 立即停止；不会把函数体里的相同字符串当成生成标记。
// 全程 []byte 操作，避免 init 全库扫描时的整文件 string 拷贝。
func IsGenerated(src []byte) bool {
	const maxGeneratedPreamble = 64 << 10
	if len(src) > maxGeneratedPreamble {
		src = src[:maxGeneratedPreamble]
	}
	for len(src) > 0 {
		src = bytes.TrimLeft(src, " \t\r\n\f\v")
		switch {
		case len(src) == 0:
			return false
		case bytes.HasPrefix(src, []byte("//")):
			line, rest, found := bytes.Cut(src, []byte("\n"))
			if generatedRe.Match(bytes.TrimSuffix(line, []byte("\r"))) {
				return true
			}
			if !found {
				return false
			}
			src = rest
		case bytes.HasPrefix(src, []byte("/*")):
			end := bytes.Index(src[2:], []byte("*/"))
			if end < 0 {
				return false
			}
			src = src[end+4:]
		default:
			return false
		}
	}
	return false
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
