// Package buildinfo 统一 CLI 与 MCP 的构建身份。
package buildinfo

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime/debug"
	"sync"
)

// Version 由 release workflow 通过 -ldflags -X 注入。go install module@version
// 没有显式注入时仍可从 Go module build info 取得版本。
var Version = "(devel)"

type Info struct {
	Version  string
	Revision string
	Dirty    bool
}

// RuntimeIdentity is captured by a daemon when it starts. The executable
// digest distinguishes two dirty builds made from the same Git revision; VCS
// metadata alone cannot do that. A running process keeps the captured value
// even if its on-disk binary is atomically upgraded later.
type RuntimeIdentity struct {
	Version          string `json:"version"`
	Revision         string `json:"revision,omitempty"`
	Dirty            bool   `json:"dirty,omitempty"`
	ExecutableSHA256 string `json:"executable_sha256,omitempty"`
}

var (
	executableDigestOnce sync.Once
	executableDigest     string
)

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

func Runtime() RuntimeIdentity {
	info := Read()
	return RuntimeIdentity{
		Version: info.Version, Revision: info.Revision, Dirty: info.Dirty,
		ExecutableSHA256: currentExecutableDigest(),
	}
}

func currentExecutableDigest() string {
	executableDigestOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			return
		}
		f, err := os.Open(exe)
		if err != nil {
			return
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err == nil {
			executableDigest = hex.EncodeToString(h.Sum(nil))
		}
	})
	return executableDigest
}

// SameRuntime reports whether two identities refer to the same executable
// generation. Prefer the content digest; fall back to build metadata for
// constrained platforms where os.Executable cannot be read.
func SameRuntime(a, b RuntimeIdentity) bool {
	if a.ExecutableSHA256 != "" && b.ExecutableSHA256 != "" {
		return a.ExecutableSHA256 == b.ExecutableSHA256
	}
	return a.Version == b.Version && a.Revision == b.Revision && a.Dirty == b.Dirty
}
