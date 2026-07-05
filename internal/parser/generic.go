package parser

import (
	"crypto/sha256"
	"encoding/hex"
)

// Generic 是通用文件级插件(2026-07-04 多语言 T0,impl §5 修订):
// 任何扩展名(config.yaml extensions 白名单,缺省关)→ 只建 file/dir 节点,
// 零符号提取。账本/经验/hook 注入/腐烂检测在文件粒度全部可用;
// 放弃的是符号粒度、格式化免疫(内容哈希:重排格式即 suspect,批量出口
// reanchor_all)与调用图(热区自动退化为纯 git 频率)。
// 覆盖 proto/SQL/前端/配置等一切文本——账本价值的大头本就常挂文件级。
type Generic struct {
	exts []string
}

// NewGeneric 建通用插件;exts 形如 [".proto", ".sql"](调用方负责去掉已被
// 专职插件占用的扩展名——Registry.Register 不做覆盖检查)。
func NewGeneric(exts []string) Generic { return Generic{exts: exts} }

func (Generic) Language() string { return "generic" }

func (g Generic) Extensions() []string { return g.exts }

// Parse 零符号:通用文件只有文件级节点(符号粒度需要真 AST,归各语言插件)。
func (Generic) Parse(path string, src []byte) ([]Symbol, error) { return nil, nil }

// HashFile 内容哈希(FileHasher 能力):无 AST 可依,任何字节变化都算变
// ——包括纯格式化(诚实的粒度代价,留痕)。
func (Generic) HashFile(src []byte) string {
	sum := sha256.Sum256(src)
	return "sha256:" + hex.EncodeToString(sum[:])
}
