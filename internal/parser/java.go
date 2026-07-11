package parser

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Java 解析器(R29-续 多语言 T1)。
//
// 同 Rust/TypeScript 决策:纯 Go 轻量词法分析(零运行时依赖,不经 javac)。
// Java 语法规整:package/import/class/interface/enum/record + 方法。
//
// 两遍扫描(比单遍状态机更可靠):
//  1. 找所有顶层 type 声明(class/interface/enum/record),记录名 + body 花括号范围;
//  2. 在每个 type body 内找方法(Type.method 规范名)。
//
// 粒度:顶层 class/interface/enum/record(type) + 类内方法。
// 取舍(宁缺不要错):泛型、注解、嵌套类精度有限;漏符号降级文件级。
//
// 哈希:Hash = sha256("jv\0"+归一化body);StructHash = 名换占位(改名免疫,迁移匹配)。
type Java struct{}

func (Java) Language() string { return "java" }

func (Java) Extensions() []string { return []string{".java"} }

func (Java) HashFile(src []byte) string {
	h := sha256.Sum256([]byte("jv\x00file\n" + string(normalizeJava(src))))
	return "sha256:" + hex.EncodeToString(h[:])
}

func (Java) Parse(path string, src []byte) ([]Symbol, error) {
	syms := scanJava(src)
	disambiguate(syms)
	return syms, nil
}

// javaType 记录一个 class/interface/enum/record 的 body 范围。
type javaType struct {
	name      string
	nameStart int
	bodyOpen  int // { 位置
	bodyClose int // } 位置
}

// scanJava 两遍扫描提取 Java 符号。
func scanJava(src []byte) []Symbol {
	var syms []Symbol
	types := findJavaTypes(src)

	// 第一遍:type 符号。
	for _, tp := range types {
		start := funcDeclStart(src, tp.nameStart)
		syms = append(syms, Symbol{
			Name: tp.name, Kind: "type",
			Start: start, End: tp.bodyClose, Body: src[start:tp.bodyClose],
			Lines: byteLines(src, start, tp.bodyClose),
		})
	}

	// 第二遍:每个 type body 内找方法。
	for _, tp := range types {
		methods := findJavaMethods(src, tp)
		syms = append(syms, methods...)
	}
	return computeHashesJava(syms)
}

// findJavaTypes 扫描顶层 type 声明(class/interface/enum/record)。
// 只取 depth=0(顶层)的——嵌套类不计(近似,宁缺)。
func findJavaTypes(src []byte) []javaType {
	var types []javaType
	i, n := 0, len(src)
	depth := 0

	for i < n {
		c := src[i]
		// 跳过字符串/字符。
		if c == '"' || c == '\'' {
			i = skipString(src, i, n)
			continue
		}
		if c == '/' && i+1 < n && (src[i+1] == '/' || src[i+1] == '*') {
			i = skipLineOrBlockComment(src, i, n)
			continue
		}
		if c == '{' {
			depth++
			i++
			continue
		}
		if c == '}' {
			if depth > 0 {
				depth--
			}
			i++
			continue
		}
		if isAlphaJava(c) && depth == 0 {
			tokStart := i
			tok, _ := readIdentAt(src, i, n)
			i = tokStart + len(tok)
			// 跳过修饰符。
			switch tok {
			case "public", "private", "protected", "static", "final", "abstract",
				"synchronized", "native", "strictfp", "default", "sealed":
				continue
			case "package", "import":
				for i < n && src[i] != ';' && src[i] != '\n' {
					i++
				}
				continue
			}
			if tok == "class" || tok == "interface" || tok == "enum" || tok == "record" {
				name, nameStart := readNextIdent(src, i, n)
				if name == "" {
					continue
				}
				i = nameStart + len(name)
				brace := findOpenBrace(src, i, n)
				if brace < 0 {
					continue
				}
				end := matchBrace(src, brace, n)
				if end < 0 {
					continue
				}
				types = append(types, javaType{name: name, nameStart: nameStart, bodyOpen: brace, bodyClose: end})
				i = brace // 让 depth 追踪吃掉 body(嵌套 class 不重复计)
			}
			continue
		}
		i++
	}
	return types
}

// findJavaMethods 在 type body 内找方法声明。
// 方法模式:[modifiers] RetType name(params) [throws ...] {body} 或 ;
func findJavaMethods(src []byte, tp javaType) []Symbol {
	var methods []Symbol
	i := tp.bodyOpen + 1
	end := tp.bodyClose
	for i < end {
		c := src[i]
		if c == '"' || c == '\'' {
			i = skipString(src, i, end)
			continue
		}
		if c == '/' && i+1 < end && (src[i+1] == '/' || src[i+1] == '*') {
			i = skipLineOrBlockComment(src, i, end)
			continue
		}
		if c == '{' {
			// 嵌套块(方法体、初始化块):跳到匹配的 }。
			cb := matchBrace(src, i, end)
			if cb < 0 {
				break
			}
			i = cb + 1
			continue
		}
		if isAlphaJava(c) {
			tokStart := i
			tok, _ := readIdentAt(src, i, end)
			i = tokStart + len(tok)
			// 跳过修饰符/void(返回类型或修饰符)。
			switch tok {
			case "public", "private", "protected", "static", "final", "abstract",
				"synchronized", "native", "strictfp", "default", "new", "return",
				"if", "for", "while", "switch", "catch", "try", "finally", "throw", "throws":
				continue
			}
			// tok 是返回类型或方法名。向前看找 "ident(" 模式。
			if m, ok := scanMethodSig(src, tok, tokStart, &i, end, tp.name); ok {
				methods = append(methods, m)
			}
			continue // ident 处理完不再 i++(i 已被 scanMethodSig/读 ident 推进)
		}
		i++
	}
	return methods
}

// scanMethodSig 从当前 tok 向前看,找"ident 后跟 ("的方法签名。
// Java 方法签名两种形式:name(params) (构造器) 或 RetType name(params)。
// 只看这两种——不多吞 token(避免字段声明的类型名吞掉后续方法名)。
func scanMethodSig(src []byte, firstTok string, firstTokStart int, i *int, end int, classNm string) (Symbol, bool) {
	saveI := *i
	// 情况 1:firstTok 本身是方法名(构造器:UserService(...))——*i 在 firstTok 后,
	// 检查 *i 处是否直接 ( (可能隔泛型/空白)。Java 除构造器外的方法必须有
	// 返回类型；若对任意 ident 都走此分支，字段初始化 `x = factory.create();`
	// 会把 create/build 等调用误建成抽象方法。构造器调用 `new Type()` 也须排除。
	if firstTok == classNm && plausibleJavaConstructor(src, firstTokStart) {
		if sym, ok := matchMethodTail(src, firstTok, firstTokStart, i, end, classNm); ok {
			return sym, true
		}
	}
	// 情况 2:firstTok 是返回类型,下一个 ident 是方法名。
	*i = saveI
	*i = skipJavaTrivia(src, *i, end)
	// 跳过泛型/数组。
	if *i < end && src[*i] == '<' {
		if gt := findChar(src, *i, end, '>'); gt > 0 {
			*i = gt + 1
			*i = skipJavaTrivia(src, *i, end)
		}
	}
	for *i+1 < end && src[*i] == '[' && src[*i+1] == ']' {
		*i += 2
		*i = skipJavaTrivia(src, *i, end)
	}
	if *i >= end || !isAlphaJava(src[*i]) {
		*i = saveI // 回退,不吞 token
		return Symbol{}, false
	}
	next, ns := readIdentAt(src, *i, end)
	*i = ns + len(next)
	if sym, ok := matchMethodTail(src, next, firstTokStart, i, end, classNm); ok {
		return sym, true
	}
	*i = saveI // 回退,不吞 token(避免吞掉字段声明后的方法)
	return Symbol{}, false
}

// plausibleJavaConstructor 区分成员构造器声明与字段初始化里的 `new Type()`。
// findJavaMethods 已跳过方法/初始化块，只需检查构造器名字之前最近的词法 token；
// new、成员访问或赋值之后的同名调用都不是声明。
func plausibleJavaConstructor(src []byte, nameStart int) bool {
	last := ""
	for i := 0; i < nameStart; {
		c := src[i]
		if c == '"' || c == '\'' {
			i = skipString(src, i, nameStart)
			last = "literal"
			continue
		}
		if c == '/' && i+1 < nameStart && (src[i+1] == '/' || src[i+1] == '*') {
			i = skipLineOrBlockComment(src, i, nameStart)
			continue
		}
		if isAlphaJava(c) {
			tok, start := readIdentAt(src, i, nameStart)
			last = tok
			i = start + len(tok)
			continue
		}
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			last = string(c)
		}
		i++
	}
	return last != "new" && last != "." && last != "=" && last != ":"
}

// matchMethodTail 从 *i 处找 (params) [throws] {body}/;——匹配则返回方法符号。
func matchMethodTail(src []byte, name string, nameStart int, i *int, end int, classNm string) (Symbol, bool) {
	*i = skipJavaTrivia(src, *i, end)
	if *i < end && src[*i] == '<' {
		if gt := findChar(src, *i, end, '>'); gt > 0 {
			*i = gt + 1
			*i = skipJavaTrivia(src, *i, end)
		}
	}
	if *i >= end || src[*i] != '(' {
		return Symbol{}, false
	}
	parEnd := matchParen(src, *i, end)
	if parEnd < 0 {
		return Symbol{}, false
	}
	*i = parEnd + 1
	*i = skipJavaTrivia(src, *i, end)
	// throws X, Y
	if *i < end && isAlphaJava(src[*i]) {
		if t, _ := readIdentAt(src, *i, end); t == "throws" {
			*i += len("throws")
			for *i < end && src[*i] != '{' && src[*i] != ';' {
				if src[*i] == '"' || src[*i] == '\'' {
					*i = skipString(src, *i, end)
					continue
				}
				if src[*i] == '/' && *i+1 < end && (src[*i+1] == '/' || src[*i+1] == '*') {
					*i = skipLineOrBlockComment(src, *i, end)
					continue
				}
				*i++
			}
		}
	}
	*i = skipJavaTrivia(src, *i, end)
	start := funcDeclStart(src, nameStart)
	if *i < end && src[*i] == '{' {
		cb := matchBrace(src, *i, end)
		if cb < 0 {
			return Symbol{}, false
		}
		*i = cb + 1
		return Symbol{
			Name: classNm + "." + name, Kind: "method",
			Start: start, End: cb, Body: src[start:cb],
			Lines: byteLines(src, start, cb),
		}, true
	}
	if *i < end && src[*i] == ';' {
		*i++
		return Symbol{
			Name: classNm + "." + name, Kind: "method",
			Start: start, End: *i, Body: src[start:*i],
			Lines: byteLines(src, start, *i),
		}, true
	}
	return Symbol{}, false
}

// readNextIdent 跳过空白读下一个标识符。
func readNextIdent(src []byte, i, n int) (string, int) {
	i = skipJavaTrivia(src, i, n)
	return readIdentAt(src, i, n)
}

func skipJavaTrivia(src []byte, i, n int) int {
	for i < n {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}
		if c == '/' && i+1 < n && (src[i+1] == '/' || src[i+1] == '*') {
			i = skipLineOrBlockComment(src, i, n)
			continue
		}
		break
	}
	return i
}

func computeHashesJava(syms []Symbol) []Symbol {
	for i := range syms {
		s := &syms[i]
		norm := normalizeJava(s.Body)
		structNorm := norm
		name := s.Name
		if dot := strings.LastIndex(name, "."); dot >= 0 {
			name = name[dot+1:]
		}
		if name != "" {
			structNorm = bytes.ReplaceAll(norm, []byte(name), []byte("_$SELF$_"))
		}
		h := sha256.Sum256([]byte("jv\x00" + s.Kind + "\x00" + string(norm)))
		sh := sha256.Sum256([]byte("jv\x00" + s.Kind + "\x00" + string(structNorm)))
		s.Hash = "sha256:" + hex.EncodeToString(h[:])
		s.StructHash = "sha256:" + hex.EncodeToString(sh[:])
	}
	return syms
}

// normalizeJava 与 JS 共用注释/字符串词法，但 Java 没有 ASI，换行和横向
// 空白都可安全归一为空格。不能直接复用 normalizeJS：后者必须为 JS 语义
// fail closed 保留 LineTerminator。
func normalizeJava(src []byte) []byte {
	var out bytes.Buffer
	pendingSpace := false
	flushSpace := func() {
		if pendingSpace && out.Len() > 0 {
			out.WriteByte(' ')
		}
		pendingSpace = false
	}
	for i, n := 0, len(src); i < n; {
		c := src[i]
		if c == '"' || c == '\'' {
			flushSpace()
			end := skipString(src, i, n)
			out.Write(src[i:end])
			i = end
			continue
		}
		if c == '/' && i+1 < n && (src[i+1] == '/' || src[i+1] == '*') {
			pendingSpace = true
			i = skipLineOrBlockComment(src, i, n)
			continue
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			pendingSpace = true
			i++
			continue
		}
		flushSpace()
		out.WriteByte(c)
		i++
	}
	return bytes.TrimSpace(out.Bytes())
}

func isAlphaJava(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == '$'
}
