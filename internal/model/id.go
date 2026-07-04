package model

import (
	"path"
	"strings"
)

// SafeRel 校验一个 repo 相对路径不逃出仓库(铁律二防线):拒绝绝对路径、
// 反斜杠、`.`/`..` 段、以及清洗后仍变化的路径。返回清洗后的正斜杠路径与是否安全。
// 节点 ID 与分片路径入库/落盘前都必须过它——否则 `../../evil.go#Foo` 能读写仓库外文件。
func SafeRel(rel string) (string, bool) {
	if rel == "" {
		return "", false
	}
	rel = ToSlash(rel)
	if strings.HasPrefix(rel, "/") || strings.Contains(rel, "\\") || strings.Contains(rel, "\x00") {
		return "", false
	}
	// Windows 盘符(c:\ / c:/):FromSlash 后会变绝对路径。
	if len(rel) >= 2 && rel[1] == ':' {
		return "", false
	}
	for seg := range strings.SplitSeq(rel, "/") {
		if seg == "." || seg == ".." {
			return "", false
		}
	}
	if path.Clean(rel) != rel {
		return "", false
	}
	return rel, true
}

// ToSlash 把路径分隔符统一为正斜杠(不依赖 filepath,model 不引平台包)。
func ToSlash(p string) string { return strings.ReplaceAll(p, "\\", "/") }

// SafeNodeID 校验节点 ID 的文件段安全(目录/项目/文件/符号节点通用)。
func SafeNodeID(id string) bool {
	file, _ := SplitNodeID(id)
	if file == "." { // 项目节点
		return true
	}
	file = strings.TrimSuffix(file, "/") // 目录节点
	_, ok := SafeRel(file)
	return ok
}

// 节点 ID 文法(impl §3 定稿):`<repo 相对路径,正斜杠>#<符号规范名>`。
// 文件节点无符号段;目录节点以 "/" 结尾;项目节点为 "."。

// FileNodeID 返回文件节点 ID(即正斜杠相对路径本身)。
func FileNodeID(file string) string { return file }

// SymbolNodeID 返回函数/decl 节点 ID。
func SymbolNodeID(file, symbol string) string { return file + "#" + symbol }

// DirNodeID 返回目录节点 ID(以 "/" 结尾;仓库根目录节点是项目节点 ".",不经此函数)。
func DirNodeID(dir string) string {
	if dir == "" || dir == "." {
		return "."
	}
	return strings.TrimSuffix(dir, "/") + "/"
}

// ProjectNodeID 是项目节点 ID。
const ProjectNodeID = "."

// SplitNodeID 拆节点 ID 为(文件路径, 符号名)。目录/项目/文件节点的符号名为空。
func SplitNodeID(id string) (file, symbol string) {
	if f, s, ok := strings.Cut(id, "#"); ok {
		return f, s
	}
	return id, ""
}

// BaseSymbol 把符号规范名的 "~n" 序号后缀剥掉(同文件多 init/_ 的重名序号,impl §3)。
func BaseSymbol(symbol string) string {
	if i := strings.LastIndexByte(symbol, '~'); i >= 0 {
		if isDigits(symbol[i+1:]) {
			return symbol[:i]
		}
	}
	return symbol
}

// LooseSymbolMatch 判断 AI 报的符号名与库内规范名是否宽松匹配
// (impl §3 定案:忽略接收者、忽略指针;精确失败后服务端做一次归一匹配)。
// got 是 AI 报名,want 是库内规范名(如 "AuthService.SignIn")。
func LooseSymbolMatch(got, want string) bool {
	// "("、")"、"*" 不会出现在 Go 标识符里,整体剔除即可归一
	// "(*AuthService).SignIn" / "*AuthService.SignIn" 这类写法。
	norm := func(s string) string {
		return strings.Map(func(r rune) rune {
			switch r {
			case '(', ')', '*':
				return -1
			}
			return r
		}, s)
	}
	g, w := norm(got), norm(want)
	if g == w {
		return true
	}
	// 忽略接收者:报 "SignIn" 可命中 "AuthService.SignIn"(唯一命中才采用,由调用方保证)。
	if i := strings.LastIndexByte(w, '.'); i >= 0 && w[i+1:] == g {
		return true
	}
	return false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
