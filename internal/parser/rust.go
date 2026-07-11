package parser

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

// Rust 解析器(R29-续 多语言 T1)。
//
// 同 TypeScript 决策:纯 Go 轻量词法分析(零运行时依赖,rustc 不经子进程调用)。
// Rust 语法比 JS 更规整——`fn`/`pub fn`/`impl Type`/`struct`/`enum`/`trait` 关键字明确,
// 花括号/圆括号配对提取符号,跳过字符串/注释防误匹配。
//
// 粒度:顶层 fn、impl 块内的方法(Type.method 规范名)、struct/enum/trait 声明(type)。
// 取舍(宁缺不要错):宏调用、复杂泛型 where 子句、嵌套 fn 精度有限;漏符号降级文件级。
//
// 哈希:Hash = sha256("rs\0"+归一化body);StructHash = 名换占位(改名免疫,迁移匹配)。
type Rust struct{}

func (Rust) Language() string { return "rust" }

func (Rust) Extensions() []string { return []string{".rs"} }

func (Rust) HashFile(src []byte) string {
	h := sha256.Sum256([]byte("rs\x00file\n" + string(normalizeRust(src))))
	return "sha256:" + hex.EncodeToString(h[:])
}

func (Rust) Parse(path string, src []byte) ([]Symbol, error) {
	syms := scanRust(src)
	disambiguate(syms)
	return syms, nil
}

// scanRust 扫描顶层 fn/struct/enum/trait 声明 + impl 块方法。
func scanRust(src []byte) []Symbol {
	var syms []Symbol
	i, n := 0, len(src)
	depth := 0     // {} 深度
	implType := "" // 当前 impl 块的类型名(depth=1 时)

	skipWSRust := func() {
		i = skipRustTrivia(src, i, n)
	}

	for i < n {
		c := src[i]
		// 跳过字符串/字符/属性(#![...])
		if c == '"' || c == '\'' {
			end := rustLiteralEnd(src, i, n)
			if end > i+1 {
				i = end
			} else {
				i++ // lifetime 引号
			}
			continue
		}
		if c == '`' {
			i = skipString(src, i, n)
			continue
		}
		if c == '/' && i+1 < n && (src[i+1] == '/' || src[i+1] == '*') {
			i = skipRustComment(src, i, n)
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
				if depth == 0 {
					implType = "" // 离开 impl 块
				}
			}
			i++
			continue
		}

		// 标识符开头,只在顶层(depth 0)或 impl 块内(depth 1)提取。
		if depth <= 1 && isAlphaUnderscore(c) {
			tokStart := i
			tok, _ := readIdentAt(src, i, n)
			i = tokStart + len(tok)

			switch tok {
			case "pub", "async", "unsafe", "extern", "const", "static":
				skipWSRust()
				continue // 修饰符,继续看下一个 token
			}

			// impl Type { ... } → 进 impl 块,记录类型名
			if tok == "impl" && depth == 0 {
				var brace int
				implType, brace = rustImplTarget(src, i, n)
				if brace < 0 || implType == "" {
					implType = ""
					continue
				}
				// 让主循环亲自吃掉 `{`，从而维持统一 depth 不变式。
				i = brace
				continue
			}

			// fn 声明
			if tok == "fn" {
				if depth == 1 && implType == "" {
					continue // 未能确认归属的块内 fn 宁缺不错挂成顶层函数
				}
				skipWSRust()
				name, nameStart := readIdentAt(src, i, n)
				if name == "" {
					continue
				}
				i = nameStart + len(name)
				sym, ok := extractRustFn(src, name, nameStart, &i, n, implType)
				if ok {
					syms = append(syms, sym)
				}
				continue
			}

			// struct / enum / trait / type → type 符号
			if tok == "struct" || tok == "enum" || tok == "trait" || tok == "type" {
				skipWSRust()
				name, nameStart := readIdentAt(src, i, n)
				if name == "" {
					continue
				}
				i = nameStart + len(name)
				sym, ok := extractRustType(src, name, nameStart, &i, n)
				if ok {
					syms = append(syms, sym)
				}
				continue
			}
			continue
		}
		i++
	}
	return computeHashesRust(syms)
}

// rustImplTarget 解析 impl 头到 body `{`，并返回实际 self type。
// `impl<T> Trait<U> for module::Type<V>` 必须归位 Type，不能误挂 Trait。
func rustImplTarget(src []byte, i, n int) (string, int) {
	i = skipRustTrivia(src, i, n)
	if i < n && src[i] == '<' {
		end := matchRustAngle(src, i, n)
		if end < 0 {
			return "", -1
		}
		i = skipRustTrivia(src, end+1, n)
	}
	targetStart := i
	targetEnd := -1
	seenWhere := false
	angleDepth, parenDepth, bracketDepth := 0, 0, 0
	for i < n {
		c := src[i]
		if c == '"' {
			i = skipRustString(src, i, n, c)
			continue
		}
		if c == '/' && i+1 < n && (src[i+1] == '/' || src[i+1] == '*') {
			i = skipRustComment(src, i, n)
			continue
		}
		if isAlphaUnderscore(c) {
			tok, start := readIdentAt(src, i, n)
			i = start + len(tok)
			if angleDepth == 0 && parenDepth == 0 && bracketDepth == 0 {
				switch tok {
				case "for":
					if !seenWhere {
						targetStart = skipRustTrivia(src, i, n)
					}
				case "where":
					if targetEnd < 0 {
						targetEnd = start
					}
					seenWhere = true
				}
			}
			continue
		}
		switch c {
		case '<':
			angleDepth++
		case '>':
			if (i == 0 || src[i-1] != '-') && angleDepth > 0 {
				angleDepth--
			}
		case '(':
			parenDepth++
		case ')':
			if parenDepth > 0 {
				parenDepth--
			}
		case '[':
			bracketDepth++
		case ']':
			if bracketDepth > 0 {
				bracketDepth--
			}
		case '{':
			if angleDepth == 0 && parenDepth == 0 && bracketDepth == 0 {
				if targetEnd < 0 {
					targetEnd = i
				}
				if targetEnd <= targetStart {
					return "", -1
				}
				return rustBaseType(src, targetStart, targetEnd), i
			}
		}
		i++
	}
	return "", -1
}

// rustBaseType 取 self type 路径在顶层泛型参数前的最后一段。
func rustBaseType(src []byte, lo, hi int) string {
	last := ""
	angleDepth := 0
	for i := lo; i < hi; {
		c := src[i]
		if c == '/' && i+1 < hi && (src[i+1] == '/' || src[i+1] == '*') {
			i = skipRustComment(src, i, hi)
			continue
		}
		if c == '<' {
			angleDepth++
			i++
			continue
		}
		if c == '>' && angleDepth > 0 {
			angleDepth--
			i++
			continue
		}
		if angleDepth == 0 && isAlphaUnderscore(c) {
			tok, start := readIdentAt(src, i, hi)
			i = start + len(tok)
			switch tok {
			case "const", "dyn", "impl", "mut", "unsafe":
			default:
				last = tok
			}
			continue
		}
		i++
	}
	return last
}

func skipRustComment(src []byte, i, n int) int {
	if i+1 >= n {
		return i + 1
	}
	if src[i+1] == '/' {
		for i < n && src[i] != '\n' {
			i++
		}
		return i
	}
	if src[i+1] != '*' {
		return i + 1
	}
	// Rust block comments 可嵌套。
	depth := 1
	i += 2
	for i < n && depth > 0 {
		if i+1 < n && src[i] == '/' && src[i+1] == '*' {
			depth++
			i += 2
			continue
		}
		if i+1 < n && src[i] == '*' && src[i+1] == '/' {
			depth--
			i += 2
			continue
		}
		i++
	}
	return i
}

func skipRustTrivia(src []byte, i, n int) int {
	for i < n {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}
		if c == '/' && i+1 < n && (src[i+1] == '/' || src[i+1] == '*') {
			i = skipRustComment(src, i, n)
			continue
		}
		break
	}
	return i
}

func matchRustAngle(src []byte, open, n int) int {
	depth := 0
	for i := open; i < n; i++ {
		c := src[i]
		if c == '"' {
			i = skipRustString(src, i, n, c) - 1
			continue
		}
		if c == '/' && i+1 < n && (src[i+1] == '/' || src[i+1] == '*') {
			i = skipRustComment(src, i, n) - 1
			continue
		}
		if c == '<' {
			depth++
		}
		if c == '>' && (i == 0 || src[i-1] != '-') {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func rustLiteralEnd(src []byte, i, n int) int {
	if i >= n {
		return n
	}
	if src[i] == '"' {
		return skipRustString(src, i, n, '"')
	}
	if src[i] != '\'' {
		return i + 1
	}
	// lifetime (`'a`) 不是 char literal。只在一个转义或一个 UTF-8 rune
	// 后立即出现闭合引号时才整段跳过。
	j := i + 1
	if j >= n {
		return j
	}
	if src[j] == '\\' {
		j += 2
	} else {
		_, size := utf8.DecodeRune(src[j:n])
		j += max(size, 1)
	}
	if j < n && src[j] == '\'' {
		return j + 1
	}
	return i + 1
}

func matchRustParen(src []byte, open, n int) int {
	return matchRustDelimited(src, open, n, '(', ')')
}

func matchRustBrace(src []byte, open, n int) int {
	return matchRustDelimited(src, open, n, '{', '}')
}

func matchRustDelimited(src []byte, open, n int, openCh, closeCh byte) int {
	depth := 0
	for i := open; i < n; {
		c := src[i]
		if c == '"' || c == '\'' {
			end := rustLiteralEnd(src, i, n)
			if end > i+1 {
				i = end
				continue
			}
		}
		if c == '/' && i+1 < n && (src[i+1] == '/' || src[i+1] == '*') {
			i = skipRustComment(src, i, n)
			continue
		}
		if c == openCh {
			depth++
		}
		if c == closeCh {
			depth--
			if depth == 0 {
				return i
			}
		}
		i++
	}
	return -1
}

func findRustOpenBrace(src []byte, i, n int) int {
	for i < n {
		c := src[i]
		if c == '"' || c == '\'' {
			end := rustLiteralEnd(src, i, n)
			if end > i+1 {
				i = end
				continue
			}
		}
		if c == '/' && i+1 < n && (src[i+1] == '/' || src[i+1] == '*') {
			i = skipRustComment(src, i, n)
			continue
		}
		if c == '{' {
			return i
		}
		if c == ';' {
			return -1
		}
		i++
	}
	return -1
}

// extractRustFn 提取 fn:从 name 起找 ( 参数 ) 和 { body }。
func extractRustFn(src []byte, name string, nameStart int, i *int, n int, implType string) (Symbol, bool) {
	*i = skipRustTrivia(src, *i, n)
	// 可选泛型 <T>
	if *i < n && src[*i] == '<' {
		gt := matchRustAngle(src, *i, n)
		if gt > 0 {
			*i = gt + 1
			*i = skipRustTrivia(src, *i, n)
		}
	}
	// 必须跟 (
	if *i >= n || src[*i] != '(' {
		return Symbol{}, false
	}
	parEnd := matchRustParen(src, *i, n)
	if parEnd < 0 {
		*i = n
		return Symbol{}, false
	}
	*i = parEnd + 1
	*i = skipRustTrivia(src, *i, n)
	// 可选返回类型 -> Type
	if *i+1 < n && src[*i] == '-' && src[*i+1] == '>' {
		*i = skipRustRetType(src, *i, n)
		*i = skipRustTrivia(src, *i, n)
	}
	// where 子句(粗略跳到 {)
	if *i < n && src[*i] == 'w' {
		if tok, _ := readIdentAt(src, *i, n); tok == "where" {
			*i += 5
			brace := findRustOpenBrace(src, *i, n)
			if brace < 0 {
				*i = n
				return Symbol{}, false
			}
			*i = brace
		}
	}
	// body { ... } 或 ; (extern fn 声明)
	if *i < n && src[*i] == '{' {
		end := matchRustBrace(src, *i, n)
		if end < 0 {
			*i = n
			return Symbol{}, false
		}
		start := funcDeclStart(src, nameStart)
		canonical := name
		kind := "func"
		if implType != "" {
			canonical = implType + "." + name
			kind = "method"
		}
		s := Symbol{
			Name: canonical, Kind: kind,
			Start: start, End: end, Body: src[start:end],
			Lines: byteLines(src, start, end),
		}
		*i = end + 1
		return s, true
	}
	if *i < n && src[*i] == ';' {
		*i++
	}
	return Symbol{}, false
}

// extractRustType 提取 struct/enum/trait/type 声明。
func extractRustType(src []byte, name string, nameStart int, i *int, n int) (Symbol, bool) {
	*i = skipRustTrivia(src, *i, n)
	// 可选泛型 <T>
	if *i < n && src[*i] == '<' {
		gt := matchRustAngle(src, *i, n)
		if gt > 0 {
			*i = gt + 1
			*i = skipRustTrivia(src, *i, n)
		}
	}
	// where 子句
	if *i < n && src[*i] == 'w' {
		if tok, _ := readIdentAt(src, *i, n); tok == "where" {
			*i += 5
		}
	}
	brace := findRustOpenBrace(src, *i, n)
	var end int
	if brace >= 0 {
		end = matchRustBrace(src, brace, n)
		if end < 0 {
			*i = n
			return Symbol{}, false
		}
	} else {
		// tuple struct Foo(...); 或 type Alias = ...;
		semi := findChar(src, *i, n, ';')
		if semi < 0 {
			*i = n
			return Symbol{}, false
		}
		end = semi
	}
	start := funcDeclStart(src, nameStart)
	s := Symbol{
		Name: name, Kind: "type",
		Start: start, End: end, Body: src[start:end],
		Lines: byteLines(src, start, end),
	}
	*i = end + 1
	return s, true
}

// skipRustString 跳过 Rust 字符串/字符字面量(含转义、原始字符串 r"..." / r#"..."#)。
func skipRustString(src []byte, i, n int, quote byte) int {
	// 原始字符串 r"..." / r#"..."# / br#"..."#(井号数配对)。
	if quote == '"' {
		prefix := i - 1
		for prefix >= 0 && src[prefix] == '#' {
			prefix--
		}
		if prefix >= 0 && src[prefix] == 'r' {
			hashCount := i - prefix - 1
			end := i + 1
			for end < n {
				if src[end] == quote {
					ok := true
					for k := 0; k < hashCount; k++ {
						if end+1+k >= n || src[end+1+k] != '#' {
							ok = false
							break
						}
					}
					if ok {
						return end + 1 + hashCount
					}
				}
				end++
			}
			return n
		}
	}
	// 普通字符串/字符:转义处理
	i++
	for i < n {
		c := src[i]
		if c == '\\' && i+1 < n {
			i += 2
			continue
		}
		if c == quote {
			return i + 1
		}
		i++
	}
	return n
}

// skipRustRetType 跳过 -> Type 到 { 或 ; 或 where。
func skipRustRetType(src []byte, i, n int) int {
	i += 2 // 跳过 ->
	depth := 0
	for i < n {
		c := src[i]
		if c == '"' || c == '\'' {
			end := rustLiteralEnd(src, i, n)
			if end > i+1 {
				i = end
				continue
			}
		}
		if c == '/' && i+1 < n && (src[i+1] == '/' || src[i+1] == '*') {
			i = skipRustComment(src, i, n)
			continue
		}
		if c == '{' && depth == 0 {
			return i
		}
		if c == ';' {
			return i
		}
		if c == '<' || c == '(' || c == '[' {
			depth++
		}
		if c == '>' || c == ')' || c == ']' {
			if depth > 0 {
				depth--
			}
		}
		if c == 'w' && depth == 0 {
			if tok, _ := readIdentAt(src, i, n); tok == "where" {
				return i
			}
		}
		i++
	}
	return i
}

// computeHashesRust 给 Rust 符号填双哈希(rs 前缀)。
func computeHashesRust(syms []Symbol) []Symbol {
	for i := range syms {
		s := &syms[i]
		norm := normalizeRust(s.Body)
		structNorm := norm
		name := s.Name
		if dot := strings.LastIndex(name, "."); dot >= 0 {
			name = name[dot+1:]
		}
		if name != "" {
			structNorm = bytes.ReplaceAll(norm, []byte(name), []byte("_$SELF$_"))
		}
		h := sha256.Sum256([]byte("rs\x00" + s.Kind + "\x00" + string(norm)))
		sh := sha256.Sum256([]byte("rs\x00" + s.Kind + "\x00" + string(structNorm)))
		s.Hash = "sha256:" + hex.EncodeToString(h[:])
		s.StructHash = "sha256:" + hex.EncodeToString(sh[:])
	}
	return syms
}

func normalizeRust(src []byte) []byte {
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
			end := rustLiteralEnd(src, i, n)
			if end > i+1 {
				flushSpace()
				out.Write(src[i:end])
				i = end
				continue
			}
		}
		if c == '/' && i+1 < n && (src[i+1] == '/' || src[i+1] == '*') {
			pendingSpace = true
			i = skipRustComment(src, i, n)
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

func isAlphaUnderscore(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}
