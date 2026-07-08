package parser

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
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
	h := sha256.Sum256([]byte("rs\x00file\n" + string(normalizeJS(src))))
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
	depth := 0 // {} 深度
	implType := "" // 当前 impl 块的类型名(depth=1 时)

	skipWSRust := func() {
		for i < n {
			c := src[i]
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				i++
			} else if c == '/' && i+1 < n && src[i+1] == '/' {
				for i < n && src[i] != '\n' {
					i++
				}
			} else if c == '/' && i+1 < n && src[i+1] == '*' {
				i += 2
				for i+1 < n && !(src[i] == '*' && src[i+1] == '/') {
					i++
				}
				i += 2
			} else {
				break
			}
		}
	}

	for i < n {
		c := src[i]
		// 跳过字符串/字符/属性(#![...])
		if c == '"' || c == '\'' {
			i = skipRustString(src, i, n, c)
			continue
		}
		if c == '`' {
			i = skipString(src, i, n)
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
				skipWSRust()
				// 可选泛型 impl<T> Trait for Type
				if i < n && src[i] == '<' {
					gt := findChar(src, i, n, '>')
					if gt > 0 {
						i = gt + 1
						skipWSRust()
					}
				}
				// 读第一个类型名(可能是 Trait)
				firstType, ftStart := readIdentAt(src, i, n)
				if firstType == "" {
					continue
				}
				i = ftStart + len(firstType)
				skipWSRust()
				// "for Type" → 真正实现类型是 for 后面那个
				if tok2, ts2 := readIdentAt(src, i, n); tok2 == "for" {
					i = ts2 + len(tok2)
					skipWSRust()
					if realType, rts := readIdentAt(src, i, n); realType != "" {
						implType = realType
						i = rts + len(realType)
					}
				} else {
					implType = firstType
				}
				// 找 impl 块的 { 并进入(depth 由后续 { } 维护)
				continue
			}

			// fn 声明
			if tok == "fn" {
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

// extractRustFn 提取 fn:从 name 起找 ( 参数 ) 和 { body }。
func extractRustFn(src []byte, name string, nameStart int, i *int, n int, implType string) (Symbol, bool) {
	skipSpacesAdvance(src, i, n)
	// 可选泛型 <T>
	if *i < n && src[*i] == '<' {
		gt := findChar(src, *i, n, '>')
		if gt > 0 {
			*i = gt + 1
			skipSpacesAdvance(src, i, n)
		}
	}
	// 必须跟 (
	if *i >= n || src[*i] != '(' {
		return Symbol{}, false
	}
	parEnd := matchParen(src, *i, n)
	if parEnd < 0 {
		*i = n
		return Symbol{}, false
	}
	*i = parEnd + 1
	skipSpacesAdvance(src, i, n)
	// 可选返回类型 -> Type
	if *i+1 < n && src[*i] == '-' && src[*i+1] == '>' {
		*i = skipRustRetType(src, *i, n)
		skipSpacesAdvance(src, i, n)
	}
	// where 子句(粗略跳到 {)
	if *i < n && src[*i] == 'w' {
		if tok, _ := readIdentAt(src, *i, n); tok == "where" {
			*i += 5
			brace := findOpenBrace(src, *i, n)
			if brace < 0 {
				*i = n
				return Symbol{}, false
			}
			*i = brace
		}
	}
	// body { ... } 或 ; (extern fn 声明)
	if *i < n && src[*i] == '{' {
		end := matchBrace(src, *i, n)
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
	skipSpacesAdvance(src, i, n)
	// 可选泛型 <T>
	if *i < n && src[*i] == '<' {
		gt := findChar(src, *i, n, '>')
		if gt > 0 {
			*i = gt + 1
			skipSpacesAdvance(src, i, n)
		}
	}
	// where 子句
	if *i < n && src[*i] == 'w' {
		if tok, _ := readIdentAt(src, *i, n); tok == "where" {
			*i += 5
		}
	}
	brace := findOpenBrace(src, *i, n)
	var end int
	if brace >= 0 {
		end = matchBrace(src, brace, n)
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
	// 原始字符串 r"..." 或 r#"..."#(井号数配对)
	if i > 0 && src[i-1] == 'r' {
		// 数 # 前缀
		hashStart := i
		for hashStart < n && src[hashStart] == '#' {
			hashStart++
		}
		hashCount := hashStart - i
		_ = hashCount // 近似:原始字符串跳到匹配的 " + 等量 #
		end := i + 1
		for end < n {
			if src[end] == quote {
				ok := true
				for k := 0; k < hashCount && end+1+k < n; k++ {
					if src[end+1+k] != '#' {
						ok = false
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
			i = skipRustString(src, i, n, c)
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
		norm := normalizeJS(s.Body)
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

func isAlphaUnderscore(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}
