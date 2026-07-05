package parser

import (
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"strings"
)

// FileCalls 是一个源文件的调用引用提取结果(impl §5 修订:全仓调用图,AST 近似)。
// 引用在这里只提取不归位——跨文件/跨包解析需要包符号表与 go.mod 模块路径,
// 属 engine 层职责;parser 保持单文件视角(插件接口语言无关)。
type FileCalls struct {
	Package    string               // package 声明名
	Imports    map[string]string    // 本地限定名 → import 路径(别名已应用;dot/blank 不入)
	Decls      []string             // 本文件符号规范名(与 Parse 同序同名,~n 消歧一致)
	Calls      map[string][]CallRef // 调用方规范名 → 出边引用(同方去重,按出现序)
	Interfaces []IfaceDecl          // 接口声明(接口→实现的方法集匹配用,2026-07-04 增)
}

// IfaceDecl 是一个接口声明的提取结果(方法名集 + 内嵌引用,归位在 engine)。
type IfaceDecl struct {
	Name    string    // 接口类型名(与 Decls 中的规范名一致)
	Methods []string  // 显式方法名(签名不参与——AST 近似,同名不同签名可能过匹配,靠 ≥2 方法阈值兜)
	Embeds  []CallRef // 内嵌接口引用(Qual=""同包 / 限定名跨包);仓外内嵌无从展开,整个接口跳过匹配
}

// CallRef 是一条未归位的调用引用。
//
// 静态近似的边界(有类型检查才能做的都不做,留痕):
//   - 接口分发经方法集匹配近似解析(engine 层,2026-07-04 codegraph 启发;
//     函数值/闭包的分发仍不解析);
//   - 链式选择器(a.b.C())不解析;
//   - 非接收者局部变量上的方法调用只带基名,归位靠包内唯一基名启发 + 接口分发兜底。
type CallRef struct {
	Qual string // 选择器限定名;"" 表示同包直呼或接收者自调
	Name string // 目标基名;接收者自调为 "Type.Method" 形式
}

// CallExtractor 是解析器插件的可选能力:提取调用引用(第一期仅 Go 实现)。
// 不并入 Parser 接口——多语言插件可以只做符号提取不做调用图。
type CallExtractor interface {
	FileCalls(path string, src []byte) (*FileCalls, error)
}

// FileCalls 提取 Go 文件的调用引用(CallExtractor 实现)。
// 规范名与 Parse 完全同法计算(接收者去指针去类型参数、~n 消歧),
// 保证 engine 拼出的 node ID 与骨架节点一致。
func (Golang) FileCalls(path string, src []byte) (*FileCalls, error) {
	fset := token.NewFileSet()
	file, err := goparser.ParseFile(fset, path, src, goparser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}

	fc := &FileCalls{
		Package: file.Name.Name,
		Imports: map[string]string{},
		Calls:   map[string][]CallRef{},
	}

	for _, imp := range file.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		local := ""
		if imp.Name != nil {
			// 别名导入;dot 导入(符号并入本包无限定名)与 blank 导入不产生限定引用。
			if imp.Name.Name == "." || imp.Name.Name == "_" {
				continue
			}
			local = imp.Name.Name
		} else {
			// 缺省限定名近似取路径末段(真实包名可与目录名不同,近似留痕;
			// 模块内包按 Go 惯例几乎总一致,不一致只是漏边不产生错边)。
			local = p[strings.LastIndexByte(p, '/')+1:]
		}
		fc.Imports[local] = p
	}

	// 声明清单:与 Parse 同序同法,含 ~n 消歧——先收 (声明, 规范名) 再统一消歧。
	type declRef struct {
		fn    *ast.FuncDecl   // 函数/方法(nil 表示 GenDecl 名)
		spec  *ast.ValueSpec  // var(nil 表示非 var)
		valAt int             // var 多名时该名在 spec.Names 的下标
	}
	var refs []declRef
	var names []string
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if d.Recv != nil {
				if base := recvBaseName(d.Recv); base != "" {
					name = base + "." + name
				}
			}
			refs = append(refs, declRef{fn: d})
			names = append(names, name)
		case *ast.GenDecl:
			switch d.Tok {
			case token.TYPE, token.VAR, token.CONST:
			default:
				continue
			}
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					refs = append(refs, declRef{})
					names = append(names, s.Name.Name)
					if it, ok := s.Type.(*ast.InterfaceType); ok {
						fc.Interfaces = append(fc.Interfaces, ifaceDecl(s.Name.Name, it))
					}
				case *ast.ValueSpec:
					for i, id := range s.Names {
						dr := declRef{}
						if d.Tok == token.VAR {
							dr.spec, dr.valAt = s, i
						}
						refs = append(refs, dr)
						names = append(names, id.Name)
					}
				}
			}
		}
	}
	// ~n 消歧与 Parse 的 disambiguate 同规则(首个不带序号)。
	count := map[string]int{}
	for i := range names {
		count[names[i]]++
		if n := count[names[i]]; n > 1 {
			names[i] = fmt.Sprintf("%s~%d", names[i], n)
		}
	}
	fc.Decls = names

	for i, dr := range refs {
		switch {
		case dr.fn != nil && dr.fn.Body != nil:
			recvIdent, recvBase := recvIdentName(dr.fn)
			fc.addCalls(names[i], dr.fn.Body, recvIdent, recvBase)
		case dr.spec != nil:
			// var 初始化表达式里的调用(len(Values)==len(Names) 时按位归属,否则整组归属)。
			if len(dr.spec.Values) == len(dr.spec.Names) {
				fc.addCalls(names[i], dr.spec.Values[dr.valAt], "", "")
			} else {
				for _, v := range dr.spec.Values {
					fc.addCalls(names[i], v, "", "")
				}
			}
		}
	}
	return fc, nil
}

// addCalls 在 AST 子树里收集 CallExpr 引用,挂到 caller 名下(同方去重)。
func (fc *FileCalls) addCalls(caller string, root ast.Node, recvIdent, recvBase string) {
	seen := map[CallRef]bool{}
	for _, ref := range fc.Calls[caller] {
		seen[ref] = true
	}
	ast.Inspect(root, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var ref CallRef
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			ref = CallRef{Name: fn.Name}
		case *ast.SelectorExpr:
			x, ok := fn.X.(*ast.Ident)
			if !ok {
				return true // 链式选择器不解析(静态近似边界)
			}
			if recvIdent != "" && x.Name == recvIdent {
				ref = CallRef{Name: recvBase + "." + fn.Sel.Name} // 接收者自调:精确归位同包方法
			} else {
				ref = CallRef{Qual: x.Name, Name: fn.Sel.Name}
			}
		default:
			return true
		}
		if ref.Name != "" && !seen[ref] {
			seen[ref] = true
			fc.Calls[caller] = append(fc.Calls[caller], ref)
		}
		return true
	})
}

// ifaceDecl 提取接口的显式方法名与内嵌引用(接口→实现匹配的原料)。
// 泛型接口的类型参数、内嵌的类型并集/约束元素(~T、A|B)不解析——那是约束不是
// 方法集,出现即整个接口跳过匹配(Embeds 记一个无法归位的哨兵,engine 会丢弃)。
func ifaceDecl(name string, it *ast.InterfaceType) IfaceDecl {
	d := IfaceDecl{Name: name}
	if it.Methods == nil {
		return d
	}
	for _, f := range it.Methods.List {
		if len(f.Names) > 0 { // 显式方法
			for _, id := range f.Names {
				d.Methods = append(d.Methods, id.Name)
			}
			continue
		}
		switch t := f.Type.(type) { // 内嵌
		case *ast.Ident:
			d.Embeds = append(d.Embeds, CallRef{Name: t.Name})
		case *ast.SelectorExpr:
			if x, ok := t.X.(*ast.Ident); ok {
				d.Embeds = append(d.Embeds, CallRef{Qual: x.Name, Name: t.Sel.Name})
			} else {
				d.Embeds = append(d.Embeds, CallRef{Name: "!unresolvable"})
			}
		default: // 约束元素(~T、A|B 等):非方法集语义,标哨兵令 engine 丢弃
			d.Embeds = append(d.Embeds, CallRef{Name: "!unresolvable"})
		}
	}
	return d
}

// recvIdentName 取方法接收者的标识符名与类型基名("(s *Store)" → "s","Store")。
func recvIdentName(fn *ast.FuncDecl) (ident, base string) {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return "", ""
	}
	if names := fn.Recv.List[0].Names; len(names) > 0 {
		ident = names[0].Name
	}
	return ident, recvBaseName(fn.Recv)
}

// Signature 从符号原文提取一行签名(展示用):函数取到 '{' 前,其余取首行。
func Signature(sym Symbol) string {
	body := string(sym.Body)
	// 跳过 doc comment 行。
	for line := range strings.SplitSeq(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*") || strings.HasPrefix(t, "*") {
			continue
		}
		if i := strings.Index(t, "{"); i > 0 {
			t = strings.TrimSpace(t[:i])
		}
		return t
	}
	return ""
}
