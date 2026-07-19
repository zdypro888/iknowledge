package parser

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
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
//   - Hash = sha256("ts\0"+kind+"\0"+归一化body):归一化 = 横向空白压缩 +
//     LineTerminator 归一为 `\n` + 去注释。JS 的 ASI/受限产生式让换行可能改变
//     语义，故只对横向格式免疫，换行保守敏感(弱于 Go 的 AST 精确重打印,留痕);
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
		if c == '/' && i+1 < n {
			// 所有深度都跳过注释；能确定是正则字面量时整段跳过，避免
			// `/class Fake {}/` 造假符号或 `/{/` 破坏花括号深度。
			if src[i+1] == '/' || src[i+1] == '*' {
				skipWS()
				continue
			}
			if end := jsRegexLiteralEnd(src, i, n, 0); end > i {
				i = end
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

		// 主扫描只取顶层声明。class 方法由 extractClass 的有界二次扫描提取；
		// 若把任意 depth=1 都当 class body，普通对象字面量
		// `const x = { fake() {} }` 会被误建成顶层 method。
		if depth == 0 && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '$') {
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
				nextTok, _ := readIdent()
				if nextTok == "function" {
					tok = "function"
				} else {
					continue
				}
			case "public", "private", "protected", "static", "readonly", "override":
				isModifier = true
				skipWS()
				continue
			case "get", "set":
				// 顶层 get/set 不是独立声明；class getter/setter 由 extractClass 处理。
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
			continue
		}
		i++
	}
	return computeHashes(syms)
}

func isIdentStart(s string) bool { return len(s) > 0 }

// extractFuncDecl 提取 function 声明:从 name 起到匹配的 } 结束。
func extractFuncDecl(src []byte, name string, nameStart int, i *int, n int) (Symbol, bool) {
	// 先完整匹配可选泛型与参数列表，再识别返回类型之后的正文。
	// 这避免把解构参数、对象默认值或对象返回类型的 `{}` 当函数体。
	afterName := *i
	brace := findJSCallableBody(src, afterName, n)
	if brace < 0 {
		// TypeScript overload/ambient declaration 合法地以 `;` 结尾。只跳过
		// 当前签名，不能把扫描指针直接置到 EOF，否则首个 overload 会让后续
		// 实现及整文件所有声明消失。找不到安全边界时至少停在名字之后，宁缺
		// 当前声明也继续扫描后续顶层 declaration。
		*i = skipJSDeclarationSignature(src, afterName, n)
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
		if c == '"' || c == '\'' || c == '`' {
			j = skipString(src, j, end)
			continue
		}
		if c == '/' && j+1 < end && (src[j+1] == '/' || src[j+1] == '*') {
			j = skipLineOrBlockComment(src, j, end)
			continue
		}
		if c == '/' {
			if regexEnd := jsRegexLiteralEnd(src, j, end, brace+1); regexEnd > j {
				j = regexEnd
				continue
			}
		}
		if c == '=' {
			// class 字段 initializer 可含 named function/object method/arrow
			// body；这些标识符都不是 class method。整段跳到字段边界，
			// 否则 `handler = function fake(){}` 会伪造 Class.fake。
			j = skipJSClassFieldInitializer(src, j+1, end)
			continue
		}
		if c == '{' {
			// 字段初始化器、static 块或嵌套类型不是方法声明。
			if close := matchBrace(src, j, end); close >= 0 {
				j = close + 1
				continue
			}
			break
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
	classSym := Symbol{
		Name: name, Kind: "type",
		Start: start, End: end, Body: body, Lines: lines,
	}
	// class 节点表达类型结构，不复制每个方法的实现。保留 extends/implements、
	// 字段与方法签名，只把已确认的直接方法正文压成空块；方法实现由独立
	// Class.method 节点锚定，修改它不能连坐 class knowledge。
	computeTypeScriptHash(&classSym, stripTypeScriptMethodBodies(src, start, end, methods))
	*i = end + 1
	return classSym, methods, true
}

// extractMethod 提取 class 方法(从 i 处的方法名起)。供 scanJSTS 内联用。
func extractMethod(src []byte, name string, nameStart int, i *int, n int) (Symbol, bool) {
	*i = nameStart
	return tryExtractMethod(src, name, nameStart, i, n)
}

// tryExtractMethod 尝试从 nameStart 处提取方法:标识符后跟 ( ... ) {。
func tryExtractMethod(src []byte, name string, nameStart int, i *int, n int) (Symbol, bool) {
	*i = nameStart + len(name)
	bodyOpen := findJSCallableBody(src, *i, n)
	// 方法体 { ... } 或抽象方法 ; 或表达式。
	if bodyOpen >= 0 {
		end := matchBrace(src, bodyOpen, n)
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
	bodyOpen := findJSCallableBody(src, nameStart+len(name), hi)
	if bodyOpen >= 0 {
		end := matchBrace(src, bodyOpen, hi)
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
		if s.Hash == "" { // class 已用剥方法正文的结构源计算
			computeTypeScriptHash(s, s.Body)
		}
	}
	return syms
}

func computeTypeScriptHash(s *Symbol, hashBody []byte) {
	norm := normalizeJS(hashBody)
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

// stripTypeScriptMethodBodies 返回 class 的哈希专用源码：保留方法签名和 `{}`，
// 删除直接方法正文。Symbol.Body 仍保持 [Start:End) 原文，不破坏 parser 契约。
func stripTypeScriptMethodBodies(src []byte, classStart, classEnd int, methods []Symbol) []byte {
	type bodyRange struct{ lo, hi int }
	ranges := make([]bodyRange, 0, len(methods))
	for _, method := range methods {
		name := method.Name
		if dot := strings.LastIndex(name, "."); dot >= 0 {
			name = name[dot+1:]
		}
		open := findJSCallableBody(src, method.Start+len(name), method.End+1)
		if open < method.Start || open >= method.End || open+1 > method.End {
			continue // 边界不确定则保留原文，宁可 class 多报 stale
		}
		ranges = append(ranges, bodyRange{lo: open + 1, hi: method.End})
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].lo < ranges[j].lo })
	var out bytes.Buffer
	cursor := classStart
	for _, r := range ranges {
		if r.lo < cursor || r.hi < r.lo || r.hi > classEnd {
			continue
		}
		out.Write(src[cursor:r.lo])
		cursor = r.hi
	}
	out.Write(src[cursor:classEnd])
	return out.Bytes()
}

// normalizeJS 归一化 JS/TS 源:横向空白压缩、去注释、LineTerminator 统一为
// 单个 `\n`。JS 的 ASI、async/生成器与 postfix 等语义面依赖换行，无法像 Go
// 一样安全地把所有空白压成空格；这里 fail closed，宁可换行 reflow 多报 suspect，
// 不可让语义变化与旧哈希碰撞。
func normalizeJS(src []byte) []byte {
	var out bytes.Buffer
	i, n := 0, len(src)
	pendingWS := false
	pendingNewline := false
	flushSpace := func() {
		if !pendingWS || out.Len() == 0 {
			pendingWS, pendingNewline = false, false
			return
		}
		if pendingNewline {
			out.WriteByte('\n')
		} else {
			out.WriteByte(' ')
		}
		pendingWS, pendingNewline = false, false
	}
	for i < n {
		c := src[i]
		// 注释。
		if c == '/' && i+1 < n && src[i+1] == '/' {
			for i < n && src[i] != '\n' {
				i++
			}
			pendingWS = true
			continue
		}
		if c == '/' && i+1 < n && src[i+1] == '*' {
			start := i
			i += 2
			for i+1 < n && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i = min(i+2, n)
			pendingWS = true
			pendingNewline = pendingNewline || bytes.IndexByte(src[start:i], '\n') >= 0
			continue
		}
		if c == '/' {
			if end := jsRegexLiteralEnd(src, i, n, 0); end > i {
				flushSpace()
				out.Write(src[i:end]) // regex 内容逐字保留；只归一外部空白
				i = end
				continue
			}
		}
		// 字符串(保留内容,只压外部空白)。
		if c == '"' || c == '\'' || c == '`' {
			flushSpace()
			end := skipString(src, i, n)
			out.Write(src[i:end])
			i = end
			continue
		}
		// 空白压成一个空格。
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			pendingWS = true
			pendingNewline = pendingNewline || c == '\n' || c == '\r'
			i++
			continue
		}
		flushSpace()
		if isJSIdentByte(c) {
			start := i
			for i < n && isJSIdentByte(src[i]) {
				i++
			}
			out.Write(src[start:i])
			continue
		}
		out.WriteByte(c)
		i++
	}
	return bytes.TrimSpace(out.Bytes())
}

// skipJSDeclarationSignature 在没有正文时跳过一个 overload/ambient 签名。
// 仅把所有括号都闭合后的 `;` 当边界；对象返回类型里的属性分号不能截断。
// 找不到确定边界时返回 start，让主扫描从函数名后继续，绝不吞掉后续文件。
func skipJSDeclarationSignature(src []byte, start, n int) int {
	parenDepth, bracketDepth, braceDepth, angleDepth := 0, 0, 0, 0
	for i := start; i < n; {
		c := src[i]
		if c == '"' || c == '\'' || c == '`' {
			i = skipString(src, i, n)
			continue
		}
		if c == '/' && i+1 < n && (src[i+1] == '/' || src[i+1] == '*') {
			i = skipLineOrBlockComment(src, i, n)
			continue
		}
		if c == '/' {
			if end := jsRegexLiteralEnd(src, i, n, start); end > i {
				i = end
				continue
			}
		}
		switch c {
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
			braceDepth++
		case '}':
			if braceDepth > 0 {
				braceDepth--
			}
		case '<':
			angleDepth++
		case '>':
			if angleDepth > 0 {
				angleDepth--
			}
		case ';':
			if parenDepth == 0 && bracketDepth == 0 && braceDepth == 0 && angleDepth == 0 {
				return i + 1
			}
		}
		i++
	}
	return start
}

// ---- 词法 helpers ----

func isJSIdentByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') || c == '_' || c == '$' || c >= 0x80
}

// skipLineOrBlockComment 跳过 JS/TS/Java 风格注释。调用者已确认 i 在 `/`。
func skipLineOrBlockComment(src []byte, i, n int) int {
	if i+1 >= n {
		return i + 1
	}
	if src[i+1] == '/' {
		for i < n && src[i] != '\n' {
			i++
		}
		return i
	}
	if src[i+1] == '*' {
		i += 2
		for i+1 < n && !(src[i] == '*' && src[i+1] == '/') {
			i++
		}
		return min(i+2, n)
	}
	return i + 1
}

// jsRegexLiteralEnd 在 slash 能确定处于“表达式起点”时跳过正则字面量，返回
// flags 之后的位置；不确定或字面量未闭合返回 slash 本身。JS 的 `/` 同时是
// 除法运算符，宁可漏跳一个复杂正则，也不能把普通除法吞成字符串。
func jsRegexLiteralEnd(src []byte, slash, n, lowerBound int) int {
	if slash < lowerBound || slash >= n || src[slash] != '/' ||
		!jsCanStartRegex(src, slash, lowerBound) {
		return slash
	}
	inClass := false
	for i := slash + 1; i < n; {
		c := src[i]
		if c == '\n' || c == '\r' || isJSUnicodeLineTerminator(src, i, n) {
			return slash // regex literal 不能跨未转义 LineTerminator
		}
		if c == '\\' {
			if i+1 >= n || src[i+1] == '\n' || src[i+1] == '\r' || isJSUnicodeLineTerminator(src, i+1, n) {
				return slash
			}
			i += 2
			continue
		}
		switch c {
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				i++
				for i < n && isJSIdentByte(src[i]) { // flags；非法 flag 留给真实编译器裁决
					i++
				}
				return i
			}
		}
		i++
	}
	return slash
}

func isJSUnicodeLineTerminator(src []byte, i, n int) bool {
	return i+2 < n && src[i] == 0xe2 && src[i+1] == 0x80 && (src[i+2] == 0xa8 || src[i+2] == 0xa9)
}

func jsCanStartRegex(src []byte, slash, lowerBound int) bool {
	j := previousJSSignificant(src, slash, lowerBound)
	if j < lowerBound {
		return true
	}
	if src[j] == ')' && jsClosesControlHead(src, j, lowerBound) {
		// `if (ok) /re/.test(x)` 等单语句控制体：`)` 平常结束表达式，
		// 但在控制头之后恰好开启一条新 Statement，slash 使用 regexp 词法目标。
		return true
	}
	switch src[j] {
	case '(', '[', '{', ',', ';', ':', '=', '!', '?', '&', '|', '*', '%', '^', '~', '<', '>':
		return true
	case '+', '-':
		// `value++ / 2` / `value-- / 2`：postfix 运算后的 slash 必为除法。
		return j == lowerBound || src[j-1] != src[j]
	}
	if !isJSIdentByte(src[j]) {
		return false
	}
	start := j
	for start > lowerBound && isJSIdentByte(src[start-1]) {
		start--
	}
	switch string(src[start : j+1]) {
	case "await", "case", "delete", "do", "else", "in", "instanceof", "new", "of",
		"return", "throw", "typeof", "void", "yield":
		return true
	}
	return false
}

// jsClosesControlHead 判断 close 是否闭合 if/while/for/with 的控制头。正向扫描
// 找匹配左括号，避免条件中的嵌套调用把简单反向计数扰乱。这里只在 slash 前一
// token 是 `)` 时触发；递归识别更早的 regex 每次都向左推进，必然终止。
func jsClosesControlHead(src []byte, close, lowerBound int) bool {
	stack := make([]int, 0, 4)
	open := -1
	for i := lowerBound; i <= close; {
		c := src[i]
		if c == '"' || c == '\'' || c == '`' {
			i = skipString(src, i, close+1)
			continue
		}
		if c == '/' && i+1 <= close && (src[i+1] == '/' || src[i+1] == '*') {
			i = skipLineOrBlockComment(src, i, close+1)
			continue
		}
		if c == '/' {
			if end := jsRegexLiteralEnd(src, i, close+1, lowerBound); end > i {
				i = end
				continue
			}
		}
		switch c {
		case '(':
			stack = append(stack, i)
		case ')':
			if len(stack) == 0 {
				return false
			}
			candidate := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if i == close {
				open = candidate
			}
		}
		i++
	}
	if open < 0 {
		return false
	}
	j := previousJSSignificant(src, open, lowerBound)
	if j < lowerBound || !isJSIdentByte(src[j]) {
		return false
	}
	start := j
	for start > lowerBound && isJSIdentByte(src[start-1]) {
		start--
	}
	switch string(src[start : j+1]) {
	case "if", "while", "for", "with":
		return true
	}
	return false
}

// previousJSSignificant 向后越过 trivia。调用点已按正向词法跳过注释；这里的
// 反向处理只用于判定 slash 前一个 token，覆盖 regex 前常见的块/行注释。
func previousJSSignificant(src []byte, before, lowerBound int) int {
	j := before - 1
	for {
		crossedLine := false
		for j >= lowerBound {
			switch src[j] {
			case ' ', '\t':
				j--
			case '\n', '\r':
				crossedLine = true
				j--
			default:
				goto triviaSkipped
			}
		}
		return j

	triviaSkipped:
		if j > lowerBound && src[j] == '/' && src[j-1] == '*' {
			if rel := bytes.LastIndex(src[lowerBound:j-1], []byte("/*")); rel >= 0 {
				j = lowerBound + rel - 1
				continue
			}
		}
		if crossedLine {
			lineStart := lowerBound
			if rel := bytes.LastIndexByte(src[lowerBound:j+1], '\n'); rel >= 0 {
				lineStart = lowerBound + rel + 1
			}
			if rel := bytes.Index(src[lineStart:j+1], []byte("//")); rel >= 0 {
				j = lineStart + rel - 1
				continue
			}
		}
		return j
	}
}

// skipJSClassFieldInitializer 跳过 class 字段 `=` 后的表达式。initializer 内的
// named function / object method / arrow body 不是 class 方法，不能交给成员扫描器。
// 有分号时精确停在分号后；无分号时仅在顶层 LineTerminator 后能确认下一个 token
// 是新成员时停下，否则保守吞到 class 末尾（漏节点优于造假节点）。
func skipJSClassFieldInitializer(src []byte, i, n int) int {
	parenDepth, bracketDepth, braceDepth := 0, 0, 0
	sawValue, canEndExpression := false, false
	for i < n {
		c := src[i]
		if c == ' ' || c == '\t' {
			i++
			continue
		}
		if c == '\n' || c == '\r' {
			if parenDepth == 0 && bracketDepth == 0 && braceDepth == 0 && sawValue && canEndExpression {
				next := skipJSTrivia(src, i+1, n)
				if next >= n || looksLikeJSClassMember(src, next, n) {
					return next
				}
			}
			i++
			continue
		}
		if c == '"' || c == '\'' || c == '`' {
			i = skipString(src, i, n)
			sawValue, canEndExpression = true, true
			continue
		}
		if c == '/' && i+1 < n && (src[i+1] == '/' || src[i+1] == '*') {
			i = skipLineOrBlockComment(src, i, n)
			continue
		}
		if c == '/' {
			if end := jsRegexLiteralEnd(src, i, n, 0); end > i {
				i = end
				sawValue, canEndExpression = true, true
				continue
			}
			canEndExpression = false // division operator
			i++
			continue
		}
		if isJSIdentByte(c) {
			for i < n && isJSIdentByte(src[i]) {
				i++
			}
			sawValue, canEndExpression = true, true
			continue
		}
		switch c {
		case '(':
			parenDepth++
			canEndExpression = false
		case ')':
			if parenDepth > 0 {
				parenDepth--
			}
			sawValue, canEndExpression = true, true
		case '[':
			bracketDepth++
			canEndExpression = false
		case ']':
			if bracketDepth > 0 {
				bracketDepth--
			}
			sawValue, canEndExpression = true, true
		case '{':
			braceDepth++
			canEndExpression = false
		case '}':
			if braceDepth == 0 {
				return i // class 自身的闭合括号，不可吞
			}
			braceDepth--
			sawValue, canEndExpression = true, true
		case ';':
			if parenDepth == 0 && bracketDepth == 0 && braceDepth == 0 {
				return i + 1
			}
			canEndExpression = false
		default:
			canEndExpression = false
		}
		i++
	}
	return n
}

func looksLikeJSClassMember(src []byte, i, n int) bool {
	if i >= n || src[i] == '}' || src[i] == '@' || src[i] == '#' {
		return true
	}
	if !isJSIdentByte(src[i]) {
		return false
	}
	tok, _ := readIdentAt(src, i, n)
	switch tok {
	case "as", "in", "instanceof", "of", "satisfies":
		return false // 可在换行后继续前一个 initializer 的运算符/类型运算
	}
	return true
}

func skipJSTrivia(src []byte, i, n int) int {
	for i < n {
		switch src[i] {
		case ' ', '\t', '\n', '\r':
			i++
			continue
		case '/':
			if i+1 < n && (src[i+1] == '/' || src[i+1] == '*') {
				i = skipLineOrBlockComment(src, i, n)
				continue
			}
		}
		break
	}
	return i
}

// findJSCallableBody 只在匹配完可选泛型、完整参数列表和可选
// TypeScript 返回类型后才返回正文 `{`。任一步不确定就拒绝建符号。
func findJSCallableBody(src []byte, afterName, n int) int {
	i := skipJSTrivia(src, afterName, n)
	if i < n && src[i] == '<' {
		angleEnd := matchAngle(src, i, n)
		if angleEnd < 0 {
			return -1
		}
		i = skipJSTrivia(src, angleEnd+1, n)
	}
	if i >= n || src[i] != '(' {
		return -1
	}
	parEnd := matchParen(src, i, n)
	if parEnd < 0 {
		return -1
	}
	i = skipJSTrivia(src, parEnd+1, n)
	if i < n && src[i] == ':' {
		return findTSBodyAfterReturnType(src, i+1, n)
	}
	if i < n && src[i] == '{' {
		return i
	}
	return -1
}

// findTSBodyAfterReturnType 识别返回类型边界。顶层 `{` 在类型尚待完成时
// 是 object/mapped type，类型已完成时才是函数体。
func findTSBodyAfterReturnType(src []byte, i, n int) int {
	i = skipJSTrivia(src, i, n)
	expectType := true
	parenDepth, bracketDepth, angleDepth := 0, 0, 0
	for i < n {
		c := src[i]
		if c == '"' || c == '\'' || c == '`' {
			i = skipString(src, i, n)
			expectType = false
			continue
		}
		if c == '/' && i+1 < n && (src[i+1] == '/' || src[i+1] == '*') {
			i = skipLineOrBlockComment(src, i, n)
			continue
		}
		if c == '/' {
			if end := jsRegexLiteralEnd(src, i, n, 0); end > i {
				i = end
				continue
			}
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}
		if isJSIdentByte(c) {
			start := i
			for i < n && isJSIdentByte(src[i]) {
				i++
			}
			switch string(src[start:i]) {
			case "keyof", "typeof", "readonly", "infer", "new", "extends", "is", "asserts":
				expectType = true
			default:
				expectType = false
			}
			continue
		}
		if c == '=' && i+1 < n && src[i+1] == '>' {
			expectType = true
			i += 2
			continue
		}
		switch c {
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
			expectType = false
		case '<':
			angleDepth++
		case '>':
			if angleDepth > 0 {
				angleDepth--
			}
			expectType = false
		case '{':
			if parenDepth > 0 || bracketDepth > 0 || angleDepth > 0 || expectType {
				close := matchBrace(src, i, n)
				if close < 0 {
					return -1
				}
				i = close + 1
				expectType = false
				continue
			}
			return i
		case '|', '&', '?', ':', ',', '=':
			expectType = true
		case ';':
			if parenDepth == 0 && bracketDepth == 0 && angleDepth == 0 {
				return -1
			}
		}
		i++
	}
	return -1
}

func matchAngle(src []byte, open, n int) int {
	depth := 0
	for i := open; i < n; i++ {
		c := src[i]
		if c == '"' || c == '\'' || c == '`' {
			i = skipString(src, i, n) - 1
			continue
		}
		if c == '/' && i+1 < n && (src[i+1] == '/' || src[i+1] == '*') {
			i = skipLineOrBlockComment(src, i, n) - 1
			continue
		}
		if c == '/' {
			if end := jsRegexLiteralEnd(src, i, n, open); end > i {
				i = end - 1
				continue
			}
		}
		if c == '<' {
			depth++
		}
		if c == '>' && (i == 0 || src[i-1] != '=') {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

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
		if c == '/' {
			if end := jsRegexLiteralEnd(src, i, n, 0); end > i {
				i = end
				continue
			}
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
		if c == '/' {
			if end := jsRegexLiteralEnd(src, i, n, open); end > i {
				i = end
				continue
			}
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
		if c == '/' && i+1 < n && (src[i+1] == '/' || src[i+1] == '*') {
			i = skipLineOrBlockComment(src, i, n)
			continue
		}
		if c == '/' {
			if end := jsRegexLiteralEnd(src, i, n, open); end > i {
				i = end
				continue
			}
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
