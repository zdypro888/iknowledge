package parser

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/build/constraint"
	goparser "go/parser"
	"go/printer"
	"go/token"
	pathpkg "path"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Golang 是 go/ast 实现的解析器插件(impl §5,第一版唯一插件,零新依赖)。
type Golang struct{}

func (Golang) Language() string { return "go" }

func (Golang) Extensions() []string { return []string{".go"} }

// selfPlaceholder 是 StructHash 里符号自身标识符的占位符(impl §5)。
const selfPlaceholder = "_$SELF$_"

// printCfg 等价 gofmt 的标准打印配置——锚定哈希的 gofmt 免疫由它保证:
// gofmt 幂等,故 print(parse(src)) == print(parse(gofmt(src)))。
var printCfg = printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}

// 双哈希定稿(impl §5)在本实现中的精确语义:
//
//		Hash          = sha256("go" \0 file-context \0 tok \0 doc.Text() \0 print(节点,注释字段全剥))
//		StructHash    = sha256("go" \0 file-context \0 tok \0            print(节点,注释字段全剥,自身名→占位符))
//		DocStructHash = sha256("go" \0 file-context \0 tok \0 normalized-doc \0 print(节点,注释字段全剥,自身名→占位符))
//
//	  - doc 以 CommentGroup.Text() 再做空白折叠(词序列)参与:注释 reflow
//	    (缩进/对齐/换行重排/// 标记)全部免疫,词级内容变更失配
//	    (doc 记录的契约变了就该重验,原意保留);
//	  - 代码经 go/printer 标准重打印:gofmt/缩进/位置移动免疫,语义变更失配;
//	  - GenDecl 一律按 Spec 打印(tok 前缀区分 var/const/type):
//	    `var a = 1` 与 `var ( a = 1 )` 的分组整理不产生伪失配、不阻断迁移;
//	  - file-context 规范化 package、build constraints 与排序后的 import
//	    alias→path;显式 alias 可按 selector 精确归位，隐式 import 因源码无法证明
//	    实际 package name 而保守纳入所有符号；import 重排免疫,但路径、blank
//	    import、build tag 改动会腐烂;
//	  - 行尾注释(ValueSpec.Comment)不参与任何哈希。
func hashParts(fileContext, tok, docText string, printed []byte) string {
	var buf bytes.Buffer
	buf.WriteString("go\x00")
	buf.WriteString(fileContext)
	buf.WriteByte(0)
	buf.WriteString(tok)
	buf.WriteByte(0)
	buf.WriteString(strings.Join(strings.Fields(docText), " ")) // 词级归一:reflow 免疫
	buf.WriteByte(0)
	buf.Write(printed)
	sum := sha256.Sum256(buf.Bytes())
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Parse 提取符号并计算双哈希(impl §5 提取规则)。
func (Golang) Parse(path string, src []byte) ([]Symbol, error) {
	fset := token.NewFileSet()
	file, err := goparser.ParseFile(fset, path, src, goparser.ParseComments|goparser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}

	semantics := newGoFileSemantics(file)
	var syms []Symbol
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			syms = append(syms, funcSymbol(fset, src, d, semantics.symbolContext(d)))
		case *ast.GenDecl:
			switch d.Tok {
			case token.TYPE, token.VAR, token.CONST:
				syms = append(syms, genSymbols(fset, src, d, semantics)...)
			}
		}
	}
	disambiguate(syms)
	return syms, nil
}

// FileHash 是文件级锚定哈希:全部符号 Hash 按顺序级联再 sha256(impl §5)——
// import 重排、格式化不再连坐 file 节点。
func FileHash(syms []Symbol) string {
	h := sha256.New()
	for _, s := range syms {
		_, _ = fmt.Fprintf(h, "%s\n", s.Hash) // hash.Hash writes never return an error
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// HashParsedFile 复用符号 Hash 级联，另用 ImportsOnly 快速读取全量文件上下文。
// 符号锚对显式 alias 只纳入实际使用者，对无法证明真实包名的隐式 import
// 保守全纳入；文件锚始终覆盖全部 import。
func (Golang) HashParsedFile(syms []Symbol, src []byte) string {
	fset := token.NewFileSet()
	file, err := goparser.ParseFile(fset, "", src, goparser.ImportsOnly|goparser.ParseComments|goparser.SkipObjectResolution)
	if err != nil {
		sum := sha256.Sum256(append([]byte("go\x00file-raw\x00"), src...))
		return "sha256:" + hex.EncodeToString(sum[:])
	}
	h := sha256.New()
	h.Write([]byte("go\x00file\x00"))
	h.Write([]byte(goFileContext(file)))
	h.Write([]byte{0})
	for _, sym := range syms {
		fmt.Fprintf(h, "%s\n", sym.Hash)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// HashFile 是 FileHasher 能力的独立出口。常规引擎路径会优先调用
// HashParsedFile 复用已有符号；直接调用本方法时才自行解析。
func (g Golang) HashFile(src []byte) string {
	syms, err := g.Parse("", src)
	if err != nil {
		sum := sha256.Sum256(append([]byte("go\x00file-raw\x00"), src...))
		return "sha256:" + hex.EncodeToString(sum[:])
	}
	return g.HashParsedFile(syms, src)
}

// funcSymbol 提取函数/方法符号。
func funcSymbol(fset *token.FileSet, src []byte, d *ast.FuncDecl, fileContext string) Symbol {
	kind := "func"
	name := d.Name.Name
	if d.Recv != nil {
		kind = "method"
		if base := recvBaseName(d.Recv); base != "" {
			name = base + "." + name // 去指针、去类型参数(impl §3 文法)
		}
	}
	start, end, lines := unitRange(fset, src, docPos(d.Doc, d.Pos()), d.End())

	// 打印时临时剥 doc(doc 以 Text() 参与哈希);单文件内串行,临时改+恢复安全。
	savedDoc := d.Doc
	d.Doc = nil
	printed := printNode(fset, d)
	savedName := d.Name.Name
	d.Name.Name = selfPlaceholder
	printedAnon := printNode(fset, d)
	d.Name.Name = savedName
	d.Doc = savedDoc

	return Symbol{
		Name: name, Kind: kind,
		Start: start, End: end,
		Body:          src[start:end],
		Lines:         lines,
		Hash:          hashParts(fileContext, kind, savedDoc.Text(), printed),
		StructHash:    hashParts(fileContext, kind, "", printedAnon),
		DocStructHash: hashParts(fileContext, kind, normalizeDocSelf(savedDoc.Text(), savedName), printedAnon),
	}
}

// genSymbols 提取 type/var/const 符号(impl §5 提取规则):
// GenDecl 按 Spec 拆符号;`var a, b int` 按名拆、共享代码单元与哈希;
// Spec 无 doc comment 时继承块级 doc。
func genSymbols(fset *token.FileSet, src []byte, d *ast.GenDecl, semantics goFileSemantics) []Symbol {
	tok := map[token.Token]string{token.TYPE: "type", token.VAR: "var", token.CONST: "const"}[d.Tok]
	grouped := d.Lparen.IsValid()

	var syms []Symbol
	for _, spec := range d.Specs {
		var names []*ast.Ident
		var specDoc *ast.CommentGroup
		switch s := spec.(type) {
		case *ast.TypeSpec:
			names, specDoc = []*ast.Ident{s.Name}, s.Doc
		case *ast.ValueSpec:
			names, specDoc = s.Names, s.Doc
		default:
			continue
		}

		// doc:自身优先,无则继承块级(impl §5);字节范围:
		// 未分组声明单元=整个 GenDecl(doc 挂 GenDecl 上);分组声明单元=该 Spec 含自身 doc
		// (块级 doc 只参与哈希,不进字节范围——否则同组多 Spec 的单元互相重叠)。
		doc := specDoc
		if doc == nil {
			doc = d.Doc
		}
		var unitFrom, unitTo token.Pos
		if grouped {
			unitFrom, unitTo = docPos(specDoc, spec.Pos()), spec.End()
		} else {
			unitFrom, unitTo = docPos(d.Doc, d.Pos()), d.End()
		}
		start, end, lines := unitRange(fset, src, unitFrom, unitTo)

		restore := stripSpecComments(spec)
		printed := printNode(fset, spec)
		fileContext := semantics.symbolContext(spec)
		hash := hashParts(fileContext, tok, doc.Text(), printed)

		syms = append(syms, make([]Symbol, len(names))...)
		out := syms[len(syms)-len(names):]
		for i, id := range names {
			savedName := id.Name
			id.Name = selfPlaceholder
			printedAnon := printNode(fset, spec)
			id.Name = savedName

			out[i] = Symbol{
				Name: id.Name, Kind: tok,
				Start: start, End: end,
				Body:          src[start:end],
				Lines:         lines,
				Hash:          hash,
				StructHash:    hashParts(fileContext, tok, "", printedAnon),
				DocStructHash: hashParts(fileContext, tok, normalizeDocSelf(doc.Text(), savedName), printedAnon),
			}
		}
		restore()
	}
	return syms
}

type goImportBinding struct {
	alias    string
	path     string
	global   bool // blank 有副作用、dot 无法静态归位，故影响文件所有符号
	implicit bool // 无显式 alias：路径末段不保证等于真实 package name
}

type goFileSemantics struct {
	base    string
	imports []goImportBinding
}

func newGoFileSemantics(file *ast.File) goFileSemantics {
	return goFileSemantics{base: goFileBaseContext(file), imports: goImportBindings(file)}
}

// symbolContext 对显式 alias 只纳入符号实际使用者；blank/dot 以及所有隐式
// import 全局纳入。Go 源码不记录隐式 import 的真实 package name，路径末段
// 只是惯例而非语义保证；若按 basename 猜别名，包名不同的依赖路径变更会让
// 符号哈希失明。这里宁可无关隐式 import 多报 suspect，也不可漏语义变更。
func (s goFileSemantics) symbolContext(node ast.Node) string {
	usedAliases := make(map[string]bool)
	ast.Inspect(node, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok {
			usedAliases[id.Name] = true
		}
		return true
	})
	selected := make([]goImportBinding, 0, len(s.imports))
	for _, imp := range s.imports {
		if imp.global || imp.implicit || usedAliases[imp.alias] {
			selected = append(selected, imp)
		}
	}
	return appendGoImports(s.base, selected)
}

// goFileContext 用于零符号文件，此时没有 AST 声明可做 import 归位，
// 因而把全量 import 纳入文件锚。
func goFileContext(file *ast.File) string {
	semantics := newGoFileSemantics(file)
	return appendGoImports(semantics.base, semantics.imports)
}

// goFileBaseContext 把 package 和 build constraints 收敛成稳定串。
func goFileBaseContext(file *ast.File) string {
	var goBuild string
	var plusBuild []string
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			continue
		}
		for _, comment := range group.List {
			line := strings.TrimSpace(comment.Text)
			if constraint.IsGoBuild(line) {
				if expr, err := constraint.Parse(line); err == nil {
					goBuild = expr.String()
				}
				continue
			}
			if constraint.IsPlusBuild(line) {
				if expr, err := constraint.Parse(line); err == nil {
					plusBuild = append(plusBuild, expr.String())
				}
			}
		}
	}
	buildExpr := goBuild
	if buildExpr == "" && len(plusBuild) > 0 {
		sort.Strings(plusBuild)
		if len(plusBuild) == 1 {
			buildExpr = plusBuild[0]
		} else {
			for i := range plusBuild {
				plusBuild[i] = "(" + plusBuild[i] + ")"
			}
			buildExpr = strings.Join(plusBuild, "&&")
		}
	}

	var out bytes.Buffer
	out.WriteString("package\x00")
	if file.Name != nil {
		out.WriteString(file.Name.Name)
	}
	out.WriteString("\nbuild\x00")
	out.WriteString(buildExpr)
	return out.String()
}

func goImportBindings(file *ast.File) []goImportBinding {
	imports := make([]goImportBinding, 0, len(file.Imports))
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			importPath = spec.Path.Value
		}
		alias := defaultGoImportAlias(importPath)
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		imports = append(imports, goImportBinding{
			alias: alias, path: importPath,
			global:   alias == "_" || alias == ".",
			implicit: spec.Name == nil,
		})
	}
	return imports
}

func defaultGoImportAlias(importPath string) string {
	base := pathpkg.Base(importPath)
	if dot := strings.LastIndex(base, ".v"); dot > 0 {
		version := base[dot+2:]
		allDigits := version != ""
		for _, c := range version {
			if c < '0' || c > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			base = base[:dot]
		}
	}
	// Go module major-version suffix (`.../pkg/v2`) 通常不是 package 名。
	if len(base) > 1 && base[0] == 'v' {
		allDigits := true
		for _, c := range base[1:] {
			if c < '0' || c > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			base = pathpkg.Base(pathpkg.Dir(importPath))
		}
	}
	return base
}

func appendGoImports(base string, imports []goImportBinding) string {
	encoded := make([]string, 0, len(imports))
	for _, imp := range imports {
		encoded = append(encoded, imp.alias+"\x00"+imp.path)
	}
	sort.Strings(encoded)
	var out strings.Builder
	out.WriteString(base)
	for _, imp := range encoded {
		out.WriteString("\nimport\x00")
		out.WriteString(imp)
	}
	return out.String()
}

// normalizeDocSelf 只替换 doc 中的完整自身标识符。这让合规的
// `// Old ...` → `// New ...` 随改名迁移，同时其他契约文字变更仍会使 DocStructHash 失配。
func normalizeDocSelf(docText, self string) string {
	if self == "" || docText == "" {
		return docText
	}
	var out strings.Builder
	for i := 0; i < len(docText); {
		r, size := utf8.DecodeRuneInString(docText[i:])
		if !goIdentRune(r) {
			out.WriteString(docText[i : i+size])
			i += size
			continue
		}
		start := i
		i += size
		for i < len(docText) {
			r2, size2 := utf8.DecodeRuneInString(docText[i:])
			if !goIdentRune(r2) {
				break
			}
			i += size2
		}
		if docText[start:i] == self {
			out.WriteString(selfPlaceholder)
		} else {
			out.WriteString(docText[start:i])
		}
	}
	return out.String()
}

func goIdentRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// printNode 用 gofmt 等价配置打印节点。注释字段已被调用方剥离,输出为纯代码。
func printNode(fset *token.FileSet, node ast.Node) []byte {
	var buf bytes.Buffer
	if err := printCfg.Fprint(&buf, fset, node); err != nil {
		// 打印失败(理论上仅畸形 AST)退回确定性错误串,不中断整文件提取。
		fmt.Fprintf(&buf, "!print-error:%v", err)
	}
	return buf.Bytes()
}

// stripSpecComments 临时摘掉 Spec 上的注释字段(Doc 与行尾 Comment),返回恢复函数。
func stripSpecComments(spec ast.Spec) func() {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		savedDoc, savedComment := s.Doc, s.Comment
		s.Doc, s.Comment = nil, nil
		return func() { s.Doc, s.Comment = savedDoc, savedComment }
	case *ast.ValueSpec:
		savedDoc, savedComment := s.Doc, s.Comment
		s.Doc, s.Comment = nil, nil
		return func() { s.Doc, s.Comment = savedDoc, savedComment }
	}
	return func() {}
}

// recvBaseName 取接收者基名:去指针、去类型参数、去括号(impl §3 文法)。
func recvBaseName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	t := recv.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
		case *ast.ParenExpr:
			t = x.X
		case *ast.IndexExpr: // Stack[T]
			t = x.X
		case *ast.IndexListExpr: // Map[K, V]
			t = x.X
		case *ast.Ident:
			return x.Name
		default:
			return ""
		}
	}
}

// docPos 返回含 doc comment 的起点。
func docPos(doc *ast.CommentGroup, fallback token.Pos) token.Pos {
	if doc != nil {
		return doc.Pos()
	}
	return fallback
}

// unitRange 把 token.Pos 区间换算为字节偏移与行号。
func unitRange(fset *token.FileSet, src []byte, from, to token.Pos) (start, end int, lines [2]int) {
	start = fset.Position(from).Offset
	end = min(fset.Position(to).Offset, len(src))
	lines = [2]int{fset.Position(from).Line, fset.Position(to).Line}
	return start, end, lines
}

// disambiguate 给同文件重复的规范名按源码出现顺序补 ~n 序号
// (多个 `func init()`、`_` 声明;首个不带序号,impl §3)。
// `~` 不是合法标识符字符,序号名不可能与真实符号撞车。
func disambiguate(syms []Symbol) {
	count := map[string]int{}
	for i := range syms {
		count[syms[i].Name]++
		if n := count[syms[i].Name]; n > 1 {
			syms[i].Name = fmt.Sprintf("%s~%d", syms[i].Name, n)
		}
	}
}
