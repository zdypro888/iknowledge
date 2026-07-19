// Package buildinfo 统一 CLI 与 MCP 的构建身份。
package buildinfo

import "runtime/debug"

// Version 由 release workflow 通过 -ldflags -X 注入。go install module@version
// 没有显式注入时仍可从 Go module build info 取得版本。
var Version = "(devel)"

type Info struct {
	Version  string
	Revision string
	Dirty    bool
}

func Read() Info {
	out := Info{Version: Version}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if out.Version == "(devel)" && bi.Main.Version != "" {
			out.Version = bi.Main.Version
		}
		for _, setting := range bi.Settings {
			switch setting.Key {
			case "vcs.revision":
				out.Revision = setting.Value
			case "vcs.modified":
				out.Dirty = setting.Value == "true"
			}
		}
	}
	if out.Version == "" {
		out.Version = "(devel)"
	}
	return out
}
