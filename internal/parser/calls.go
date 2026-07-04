package parser

import (
	"go/ast"
	goparser "go/parser"
	"go/token"
	"strings"
)

// SameFileCalls 提取同文件内的静态调用关系(impl §5:第一期只做同文件内,
// 全仓调用图留给后期,避免类型检查)。返回 规范名 → 它调用的同文件符号规范名列表。
// auto 部分不落盘,读取时现算(impl §3)。
func SameFileCalls(path string, src []byte) (map[string][]string, error) {
	fset := token.NewFileSet()
	file, err := goparser.ParseFile(fset, path, src, goparser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}

	// 同文件可调用目标:函数名 + 方法名(基名)。
	declared := map[string]string{} // 基名 → 规范名
	for _, decl := range file.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok {
			name := fd.Name.Name
			canonical := name
			if fd.Recv != nil {
				if base := recvBaseName(fd.Recv); base != "" {
					canonical = base + "." + name
				}
			}
			// 方法与函数同名时函数优先(方法调用通常带 receiver 前缀,难以静态归一)。
			if _, exists := declared[name]; !exists || fd.Recv == nil {
				declared[name] = canonical
			}
		}
	}

	calls := map[string][]string{}
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		caller := fd.Name.Name
		if fd.Recv != nil {
			if base := recvBaseName(fd.Recv); base != "" {
				caller = base + "." + caller
			}
		}
		seen := map[string]bool{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				name = fn.Name
			case *ast.SelectorExpr:
				name = fn.Sel.Name // x.Method():按基名匹配(近似,一期够用)
			}
			if canonical, ok := declared[name]; ok && canonical != caller && !seen[canonical] {
				seen[canonical] = true
				calls[caller] = append(calls[caller], canonical)
			}
			return true
		})
	}
	return calls, nil
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
