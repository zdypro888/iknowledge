package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/zdypro888/iknowledge/internal/store"
)

const (
	// SemanticAPIKeyEnv 是唯一会被语义 provider 读取的密钥环境变量。
	// 仓库不能选择任意环境变量名，避免恶意配置把其他凭据送往攻击者端点。
	SemanticAPIKeyEnv = "IKNOWLEDGE_EMBEDDING_API_KEY"

	semanticSettingsSchema = 1
	semanticDefaultTopK    = 20
	semanticDefaultMin     = 0.35
	semanticDefaultMaxMiB  = 512
	semanticDefaultTimeout = 30
)

// SemanticSettings 是 canonical repo 对应的本机私有配置，不属于知识正本，
// 不进入 Git 或 bundle。Enabled 只有经本机 CLI 显式写入才可能为 true。
type SemanticSettings struct {
	Schema       int     `json:"schema"`
	Enabled      bool    `json:"enabled"`
	Endpoint     string  `json:"endpoint"`
	Model        string  `json:"model"`
	Dimensions   int     `json:"dimensions,omitempty"`
	Revision     string  `json:"revision,omitempty"`
	TopK         int     `json:"top_k"`
	MinScore     float64 `json:"min_score"`
	MaxVectorMiB int     `json:"max_vector_mib"`
	TimeoutSec   int     `json:"timeout_seconds"`
}

// DefaultSemanticSettings 返回禁用态默认值。
func DefaultSemanticSettings() SemanticSettings {
	return SemanticSettings{
		Schema: semanticSettingsSchema, TopK: semanticDefaultTopK,
		MinScore: semanticDefaultMin, MaxVectorMiB: semanticDefaultMaxMiB,
		TimeoutSec: semanticDefaultTimeout,
	}
}

// LoadSemanticSettings 读取仓外配置。不存在视为安全的 disabled，而不是错误。
func LoadSemanticSettings(s *store.Store) (SemanticSettings, error) {
	cfg := DefaultSemanticSettings()
	data, err := s.LoadSemanticConfig()
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return DefaultSemanticSettings(), fmt.Errorf("解析本机 semantic 配置: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return DefaultSemanticSettings(), fmt.Errorf("解析本机 semantic 配置: 尾随 JSON")
	}
	normalizeSemanticSettings(&cfg)
	if err := ValidateSemanticSettings(cfg); err != nil {
		return DefaultSemanticSettings(), err
	}
	return cfg, nil
}

// SaveSemanticSettings 校验并原子写入仓外私有状态。
func SaveSemanticSettings(s *store.Store, cfg SemanticSettings) error {
	normalizeSemanticSettings(&cfg)
	if err := ValidateSemanticSettings(cfg); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return s.WriteSemanticConfig(data)
}

func normalizeSemanticSettings(cfg *SemanticSettings) {
	cfg.Endpoint = strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.Revision = strings.TrimSpace(cfg.Revision)
}

// ValidateSemanticSettings 校验本机 provider 配置。HTTP 只允许 loopback；
// 远程 endpoint 必须 HTTPS，且 URL 不得携带 userinfo/query/fragment。
func ValidateSemanticSettings(cfg SemanticSettings) error {
	if cfg.Schema != semanticSettingsSchema {
		return fmt.Errorf("semantic 配置 schema=%d，当前仅支持 %d", cfg.Schema, semanticSettingsSchema)
	}
	if cfg.TopK < 1 || cfg.TopK > 100 {
		return fmt.Errorf("semantic top_k=%d 越界(1..100)", cfg.TopK)
	}
	if cfg.MinScore < 0 || cfg.MinScore > 1 {
		return fmt.Errorf("semantic min_score=%g 越界(0..1)", cfg.MinScore)
	}
	if cfg.MaxVectorMiB < 16 || cfg.MaxVectorMiB > 512 {
		return fmt.Errorf("semantic max_vector_mib=%d 越界(16..512)", cfg.MaxVectorMiB)
	}
	if cfg.TimeoutSec < 1 || cfg.TimeoutSec > 30 {
		return fmt.Errorf("semantic timeout_seconds=%d 越界(1..30)", cfg.TimeoutSec)
	}
	if cfg.Dimensions < 0 || cfg.Dimensions > 4096 {
		return fmt.Errorf("semantic dimensions=%d 越界(0..4096)", cfg.Dimensions)
	}
	if len(cfg.Revision) > 256 || hasControl(cfg.Revision) {
		return fmt.Errorf("semantic revision 非法或过长")
	}
	if !cfg.Enabled && cfg.Endpoint == "" && cfg.Model == "" {
		return nil
	}
	if cfg.Endpoint == "" || cfg.Model == "" {
		return fmt.Errorf("semantic endpoint 与 model 必须同时配置")
	}
	if len(cfg.Model) > 256 || hasControl(cfg.Model) {
		return fmt.Errorf("semantic model 非法或过长")
	}
	return validateSemanticEndpoint(cfg.Endpoint)
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

func validateSemanticEndpoint(endpoint string) error {
	if len(endpoint) > 4096 || hasControl(endpoint) {
		return fmt.Errorf("semantic endpoint 非法或过长")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("semantic endpoint 不是完整 URL")
	}
	if u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("semantic endpoint 不得包含 userinfo、query 或 fragment")
	}
	if u.RawPath != "" || strings.Contains(u.Path, "//") || strings.HasSuffix(u.Host, ":") {
		return fmt.Errorf("semantic endpoint 路径或主机格式不规范")
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("semantic endpoint 路径不得包含点段")
		}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("semantic endpoint 仅支持 http/https")
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" || strings.Contains(host, "%") {
		return fmt.Errorf("semantic endpoint 缺主机")
	}
	loopback := semanticEndpointLoopback(endpoint)
	if u.Scheme == "http" && !loopback {
		return fmt.Errorf("远程 semantic endpoint 必须使用 https；http 仅允许 loopback")
	}
	return nil
}

func semanticEndpointLoopback(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := strings.TrimSpace(u.Hostname())
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SemanticSettingsFingerprint 标识向量空间与预处理契约；API key 不参与。
func SemanticSettingsFingerprint(cfg SemanticSettings) string {
	normalizeSemanticSettings(&cfg)
	fields := []string{
		"iknowledge-semantic-settings-v1", cfg.Endpoint, cfg.Model,
		strconv.Itoa(cfg.Dimensions), cfg.Revision,
		"summary-entry+era-summary", "semantic-source-v2", "redact-v1", "document-4k-v1", "l2-v1",
	}
	sum := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
	return "v1:" + hex.EncodeToString(sum[:])
}
