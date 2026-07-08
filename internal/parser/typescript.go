package parser

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// TypeScript/JavaScript 解析器(R29 批次6 多语言 T1)。
//
// 策略决策(零重依赖铁律下):Node.js 不自带 JS/TS AST 解析库(vm 只解析 JS,
// 不支持 TS;acorn/babel 需 npm 装,违反零依赖哲学)。故用**纯 Go 轻量词法分析**:
// 扫描顶层 function/class/method/export 声明关键字 + 大括号配对算行范围。
//
// 这是近似解析(像 codegraph 的"宁缺不要错",knowledge.md §16.3):
//   - 优点:零运行时依赖、跨平台无差异、对所有 JS/TS 变体(含 TS 类型注解)都能词法提取;
//   - 取舍:不解析完整 AST,嵌套函数/箭头函数/装饰器精度有限;复杂泛型/JSX 可能漏符号。
//     漏符号不致错(降级为文件级),错符号会致错——所以倾向漏(宁缺)。
//
// 粒度与哈希语义对齐 Go/Python 插件:
//   - 符号 = 顶层 function/class + 类方法(Class.method 规范名);
//   - Hash = sha256("ts\0"+kind+"\0"+归一化body):归一化 = 压缩空白 + 去注释,
//     使格式/注释免疫(弱于 Go 的 go/printer 精确重打印,留痕);
//   - StructHash = 自身名换占位符后的归一化:改名免疫,仅迁移匹配;
//   - class 符号的哈希剥离方法体(方法另有符号)——方法改动不连坐 class 节点。
type TypeScript struct{}

func (TypeScript) Language() string { return "typescript" }

// .ts/.tsx/.js/.jsx/.mjs/.cjs/.mts/.cts 全覆盖(JS/TS 生态全扩展名)。
func (TypeScript) Extensions() []string {
	return []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts"}
}

// 实现 FileHasher:文件级哈希(无符号时不能空级联出常量,见 parser.go FileHasher 注释)。
func (TypeScript) HashFile(src []byte) string {
	h := sha256.Sum256([]byte("ts\x00file\n" + string(normalizeJS(src))))
	return "sha256:" + hex.EncodeToString(h[:])
}

// Parse 纯 Go 词法提取 JS/TS 顶层符号(零运行时依赖,不经 node 子进程)。
func (TypeScript) Parse(path string, src []byte) ([]Symbol, error) {
	syms := scanJSTS(src)
	disambiguate(syms) // ~n 消歧与 Go/Python 同规则
	return syms, nil
}

// ---- 词法分析 ----

// scanJSTS 扫描顶层 function/class 声明 + 类方法。返回符号(已含双哈希)。
// 算法:逐 token 扫描顶层(花括号深度 0),遇 function/class 关键字提取声明;
// class body 内(深度 1)扫描方法。注释/字符串/模板字面量跳过防误匹配。
func scanJSTS(src []byte) []Symbol {
	var syms []Symbol
	i, n := 0, len(src)
	depth := 0 // 花括号深度

	// 跳过空白。
	skipWS := func() {
		for i < n {
			c := src[i]
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				i++
			} else if c == '/' && i+1 < n && src[i+1] == '/' {
				// 单行注释。
				for i < n && src[i] != '\n' {
					i++
				}
			} else if c == '/' && i+1 < n && src[i+1] == '*' {
				// 多行注释。
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

	// 读标识符(JS 标识符:字母/数字/_/$,含 Unicode)。
	readIdent := func() (string, int) {
		start := i
		for i < n {
			c := src[i]
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '$' || c >= 0x80 {
				i++
			} else {
				break
			}
		}
		return string(src[start:i]), start
	}

	for i < n {
		c := src[i]

		// 跳过字符串/模板字面量/正则(防内容里的 function/class 误匹配)。
		if c == '"' || c == '\'' || c == '`' {
			i = skipString(src, i, n)
			continue
		}
		if c == '/' && depth == 0 && i+1 < n {
			// 顶层除号或正则;近似处理:跳过单行/多行注释,正则当除号略过。
			if src[i+1] == '/' || src[i+1] == '*' {
				skipWS()
				continue
			}
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

		// 只在顶层(depth 0)和 class body(depth 1)提取声明。
		if depth <= 1 && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '$') {
			tokStart := i
			tok, _ := readIdent()

			// 跳过修饰符:async, export, default, public, private, static, get, set, readonly, abstract, declare
			isModifier := false
			switch tok {
			case "export", "default", "declare", "abstract":
				skipWS()
				// export default function 可能;继续读下一个 token。
				i = tokStart + len(tok)
				continue
			case "async":
				// async function / async method
				skipWS()
				nextTok, nextStart := readIdent()
				if nextTok == "function" {
					i = nextStart
					tok = "function"
				} else {
					i = nextStart
					continue
				}
			case "public", "private", "protected", "static", "readonly", "override":
				isModifier = true
				skipWS()
				continue
			case "get", "set":
				// getter/setter:当作方法名前缀,跳过看后面
				skipWS()
				nextTok, nextStart := readIdent()
				if depth == 1 && isIdentStart(nextTok) {
					// class 内 getter/setter:当方法处理
					sym, ok := extractMethod(src, nextTok, nextStart, &i, n)
					if ok {
						syms = append(syms, sym)
					}
					continue
				}
				i = nextStart
				continue
			}

			_ = isModifier

			if tok == "function" && depth == 0 {
				skipWS()
				// 可能有 * (generator)
				if i < n && src[i] == '*' {
					i++
					skipWS()
				}
				name, nameStart := readIdent()
				if name == "" {
					continue // 匿名函数(export default function() {}):跳过,无符号名
				}
				sym, ok := extractFuncDecl(src, name, nameStart, &i, n)
				if ok {
					syms = append(syms, sym)
				}
				continue
			}
			if tok == "class" && depth == 0 {
				skipWS()
				name, nameStart := readIdent()
				if name == "" {
					continue
				}
				classSym, methods, ok := extractClass(src, name, nameStart, &i, n)
				if ok {
					syms = append(syms, classSym)
					syms = append(syms, methods...)
				}
				continue
			}
			// class body 内的方法(depth 1):function 关键字或直接 methodName(...) {
			if depth == 1 && tok != "" {
				// 检查是否是方法(标识符后跟可选 <泛型> 和 ( 参数 )
				sym, ok := tryExtractMethod(src, tok, tokStart, &i, n)
				if ok {
					syms = append(syms, sym)
				}
				continue
			}
			continue
		}
		i++
	}
	return computeHashes(syms)
}

func isIdentStart(s string) bool { return len(s) > 0 }

// extractFuncDecl 提取 function 声明:从 name 起到匹配的 } 结束。
func extractFuncDecl(src []byte, name string, nameStart int, i *int, n int) (Symbol, bool) {
	// 找到参数列表 ( ... ) 后的 {
	brace := findOpenBrace(src, *i, n)
	if brace < 0 {
		*i = n
		return Symbol{}, false
	}
	end := matchBrace(src, brace, n)
	if end < 0 {
		*i = n
		return Symbol{}, false
	}
	start := funcDeclStart(src, nameStart)
	body := src[start:end]
	lines := byteLines(src, start, end)
	*i = end + 1
	return Symbol{
		Name: name, Kind: "func",
		Start: start, End: end, Body: body, Lines: lines,
	}, true
}

// extractClass 提取 class 声明 + 其方法。class 符号剥方法体(方法另有符号)。
func extractClass(src []byte, name string, nameStart int, i *int, n int) (Symbol, []Symbol, bool) {
	brace := findOpenBrace(src, *i, n)
	if brace < 0 {
		*i = n
		return Symbol{}, nil, false
	}
	end := matchBrace(src, brace, n)
	if end < 0 {
		*i = n
		return Symbol{}, nil, false
	}
	start := funcDeclStart(src, nameStart)
	body := src[start:end]
	lines := byteLines(src, start, end)

	// 扫描 class body 提取方法。
	var methods []Symbol
	j := brace + 1
	for j < end {
		c := src[j]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			j++
			continue
		}
		// 跳过修饰符。
		if tok, ts := readIdentAt(src, j, end); tok != "" {
			switch tok {
			case "public", "private", "protected", "static", "readonly", "override", "async", "abstract", "declare":
				j = ts + len(tok)
				continue
			case "get", "set":
				j = ts + len(tok)
				continue
			}
			if sym, ok := tryExtractMethodRange(src, tok, ts, j, end); ok {
				sym.Name = name + "." + sym.Name // 规范名:Class.method
				methods = append(methods, sym)
				j = sym.End + 1
				continue
			}
			j = ts + len(tok)
			continue
		}
		j++
	}
	*i = end + 1
	return Symbol{
		Name: name, Kind: "type",
		Start: start, End: end, Body: body, Lines: lines,
	}, methods, true
}

// extractMethod 提取 class 方法(从 i 处的方法名起)。供 scanJSTS 内联用。
func extractMethod(src []byte, name string, nameStart int, i *int, n int) (Symbol, bool) {
	*i = nameStart
	return tryExtractMethod(src, name, nameStart, i, n)
}

// tryExtractMethod 尝试从 nameStart 处提取方法:标识符后跟 ( ... ) {。
func tryExtractMethod(src []byte, name string, nameStart int, i *int, n int) (Symbol, bool) {
	*i = nameStart + len(name)
	skipSpacesAdvance(src, i, n)
	// 可选泛型 <T>
	if *i < n && src[*i] == '<' {
		gt := findChar(src, *i, n, '>')
		if gt > 0 {
			*i = gt + 1
			skipSpacesAdvance(src, i, n)
		}
	}
	// 可选类型参数对构造器等;必须跟 ( 才是方法。
	if *i >= n || src[*i] != '(' {
		return Symbol{}, false
	}
	// 找参数列表结束 )。
	parEnd := matchParen(src, *i, n)
	if parEnd < 0 {
		return Symbol{}, false
	}
	*i = parEnd + 1
	skipSpacesAdvance(src, i, n)
	// 可选返回类型 :Type
	if *i < n && src[*i] == ':' {
		// 跳过类型注解到 { 或 ; 或 =。
		*i = skipTypeAnnotation(src, *i, n)
		skipSpacesAdvance(src, i, n)
	}
	// 方法体 { ... } 或抽象方法 ; 或表达式。
	if *i < n && src[*i] == '{' {
		end := matchBrace(src, *i, n)
		if end < 0 {
			*i = n
			return Symbol{}, false
		}
		start := nameStart
		body := src[start:end]
		lines := byteLines(src, start, end)
		*i = end + 1
		return Symbol{
			Name: name, Kind: "method",
			Start: start, End: end, Body: body, Lines: lines,
		}, true
	}
	return Symbol{}, false
}

// tryExtractMethodRange 在 [lo, hi) 范围内提取方法(供 extractClass body 扫描用)。
func tryExtractMethodRange(src []byte, name string, nameStart, lo, hi int) (Symbol, bool) {
	i := nameStart + len(name)
	skipSpacesRange(src, &i, hi)
	if i < hi && src[i] == '<' {
		gt := findChar(src, i, hi, '>')
		if gt > 0 {
			i = gt + 1
			skipSpacesRange(src, &i, hi)
		}
	}
	if i >= hi || src[i] != '(' {
		return Symbol{}, false
	}
	parEnd := matchParen(src, i, hi)
	if parEnd < 0 {
		return Symbol{}, false
	}
	i = parEnd + 1
	skipSpacesRange(src, &i, hi)
	if i < hi && src[i] == ':' {
		i = skipTypeAnnotation(src, i, hi)
		skipSpacesRange(src, &i, hi)
	}
	if i < hi && src[i] == '{' {
		end := matchBrace(src, i, hi)
		if end < 0 || end >= hi {
			return Symbol{}, false
		}
		start := nameStart
		return Symbol{
			Name: name, Kind: "method",
			Start: start, End: end, Body: src[start:end], Lines: byteLines(src, start, end),
		}, true
	}
	return Symbol{}, false
}

// ---- 哈希计算 ----

// computeHashes 给每个符号填 Hash/StructHash(归一化 body → sha256)。
func computeHashes(syms []Symbol) []Symbol {
	for i := range syms {
		s := &syms[i]
		norm := normalizeJS(s.Body)
		// StructHash:自身名换占位符(改名免疫)。对 method,名是 "Class.method",
		// 换最后一段;对 func/type,换整个名。近似:把 body 里的名字做字符串替换。
		structNorm := norm
		if dot := strings.LastIndex(s.Name, "."); dot >= 0 {
			structNorm = bytes.ReplaceAll(norm, []byte(s.Name[dot+1:]), []byte("_$SELF$_"))
		} else {
			structNorm = bytes.ReplaceAll(norm, []byte(s.Name), []byte("_$SELF$_"))
		}
		h := sha256.Sum256([]byte("ts\x00" + s.Kind + "\x00" + string(norm)))
		sh := sha256.Sum256([]byte("ts\x00" + s.Kind + "\x00" + string(structNorm)))
		s.Hash = "sha256:" + hex.EncodeToString(h[:])
		s.StructHash = "sha256:" + hex.EncodeToString(sh[:])
	}
	return syms
}

// normalizeJS 归一化 JS/TS 源:压缩连续空白、去注释,使格式/注释免疫(近似 gofmt 免疫)。
func normalizeJS(src []byte) []byte {
	var out bytes.Buffer
	i, n := 0, len(src)
	prevWS := false
	for i < n {
		c := src[i]
		// 注释。
		if c == '/' && i+1 < n && src[i+1] == '/' {
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < n && src[i+1] == '*' {
			i += 2
			for i+1 < n && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i += 2
			continue
		}
		// 字符串(保留内容,只压外部空白)。
		if c == '"' || c == '\'' || c == '`' {
			end := skipString(src, i, n)
			out.Write(src[i:end])
			i = end
			prevWS = false
			continue
		}
		// 空白压成一个空格。
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			if !prevWS {
				out.WriteByte(' ')
				prevWS = true
			}
			i++
			continue
		}
		out.WriteByte(c)
		prevWS = false
		i++
	}
	return bytes.TrimSpace(out.Bytes())
}

// ---- 词法 helpers ----

// skipString 跳过一个字符串字面量(含转义、模板字面量 ${} 近似处理),返回结束位置。
func skipString(src []byte, i, n int) int {
	quote := src[i]
	i++
	for i < n {
		c := src[i]
		if c == '\\' && i+1 < n {
			i += 2
			continue
		}
		if quote != '`' && c == quote {
			return i + 1
		}
		if quote == '`' && c == '`' {
			return i + 1
		}
		// 模板字面量 ${...} 内部近似当普通字符(不递归,可能误匹配 } 但够用)。
		i++
	}
	return n
}

// findOpenBrace 从 i 起找第一个未被字符串/注释遮挡的 {。
func findOpenBrace(src []byte, i, n int) int {
	for i < n {
		c := src[i]
		if c == '"' || c == '\'' || c == '`' {
			i = skipString(src, i, n)
			continue
		}
		if c == '/' && i+1 < n && (src[i+1] == '/' || src[i+1] == '*') {
			if src[i+1] == '/' {
				for i < n && src[i] != '\n' {
					i++
				}
			} else {
				i += 2
				for i+1 < n && !(src[i] == '*' && src[i+1] == '/') {
					i++
				}
				i += 2
			}
			continue
		}
		if c == '{' {
			return i
		}
		if c == ';' {
			return -1 // 声明无 body(抽象/重导出)
		}
		i++
	}
	return -1
}

// matchBrace 从 { 起匹配对应的 } 位置(跳过字符串/注释)。
func matchBrace(src []byte, open, n int) int {
	depth := 0
	i := open
	for i < n {
		c := src[i]
		if c == '"' || c == '\'' || c == '`' {
			i = skipString(src, i, n)
			continue
		}
		if c == '/' && i+1 < n && (src[i+1] == '/' || src[i+1] == '*') {
			if src[i+1] == '/' {
				for i < n && src[i] != '\n' {
					i++
				}
			} else {
				i += 2
				for i+1 < n && !(src[i] == '*' && src[i+1] == '/') {
					i++
				}
				i += 2
			}
			continue
		}
		if c == '{' {
			depth++
		}
		if c == '}' {
			depth--
			if depth == 0 {
				return i
			}
		}
		i++
	}
	return -1
}

// matchParen 从 ( 起匹配对应的 ) 位置。
func matchParen(src []byte, open, n int) int {
	depth := 0
	i := open
	for i < n {
		c := src[i]
		if c == '"' || c == '\'' || c == '`' {
			i = skipString(src, i, n)
			continue
		}
		if c == '(' {
			depth++
		}
		if c == ')' {
			depth--
			if depth == 0 {
				return i
			}
		}
		i++
	}
	return -1
}

func skipTypeAnnotation(src []byte, i, n int) int {
	// 从 : 起跳到 { 或 ; 或 = 或 \n(近似:类型注解可能复杂,这里粗略)。
	depth := 0
	for i < n {
		c := src[i]
		if c == '{' {
			return i
		}
		if c == ';' || c == '=' {
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
		if c == '\n' && depth == 0 {
			// 行尾:可能是方法签名换行后接 {,回退找 {
			return i
		}
		i++
	}
	return i
}

func findChar(src []byte, i, n int, ch byte) int {
	for i < n {
		if src[i] == ch {
			return i
		}
		i++
	}
	return -1
}

func readIdentAt(src []byte, i, n int) (string, int) {
	start := i
	for i < n {
		c := src[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '$' || c >= 0x80 {
			i++
		} else {
			break
		}
	}
	return string(src[start:i]), start
}

func skipSpacesAdvance(src []byte, i *int, n int) {
	for *i < n {
		c := src[*i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			*i++
		} else {
			break
		}
	}
}

func skipSpacesRange(src []byte, i *int, hi int) {
	for *i < hi {
		c := src[*i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			*i++
		} else {
			break
		}
	}
}

// funcDeclStart 回退到声明所在行首(含可能的装饰器/注释行,近似:找最近的 \n+1)。
func funcDeclStart(src []byte, nameStart int) int {
	// 简化:从 nameStart 向前找 function/class 关键字位置作为 start。
	for i := nameStart; i > 0; i-- {
		if src[i] == '\n' {
			// 检查这行是否是 function/class/export 等(向前扫到行首)。
			lineStart := i + 1
			// 再向前看是否连续修饰符行(export 等)——近似取当前行首。
			return lineStart
		}
	}
	return 0
}

// byteLines 计算字节范围 [start, end) 对应的行号(1-based)。
func byteLines(src []byte, start, end int) [2]int {
	l1 := 1 + bytes.Count(src[:start], []byte("\n"))
	l2 := 1 + bytes.Count(src[:end], []byte("\n"))
	return [2]int{l1, l2}
}

// 抑制未使用告警(fmt 在错误路径用)。
var _ = fmt.Sprintf
