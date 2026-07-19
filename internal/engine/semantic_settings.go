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

	"github.com/zdypro888/iknowledge/internal/semantic"
	"github.com/zdypro888/iknowledge/internal/store"
)

const (
	// SemanticAPIKeyEnv 是唯一会被语义 provider 读取的密钥环境变量。
	// 仓库不能选择任意环境变量名，避免恶意配置把其他凭据送往攻击者端点。
	SemanticAPIKeyEnv = "IKNOWLEDGE_EMBEDDING_API_KEY"
	// SemanticAPIOriginEnv is the non-secret, process-level audience binding for
	// SemanticAPIKeyEnv. A remote key is never sent unless this canonical origin
	// matches the configured endpoint, preventing one multi-repo daemon from
	// accidentally reusing repo A's credential at repo B's provider.
	SemanticAPIOriginEnv = "IKNOWLEDGE_EMBEDDING_API_ORIGIN"

	semanticSettingsSchema = 1
	semanticDefaultTopK    = 20
	semanticDefaultMin     = 0.35
	semanticDefaultMaxMiB  = 512
	semanticDefaultTimeout = 30
)

// SemanticRebuildPolicy records who the user authorizes to explicitly
// synchronize the derived semantic index. It never starts a background job.
type SemanticRebuildPolicy string

const (
	SemanticRebuildManual   SemanticRebuildPolicy = "manual"
	SemanticRebuildAILocal  SemanticRebuildPolicy = "ai-local"
	SemanticRebuildAIRemote SemanticRebuildPolicy = "ai-remote"
)

// CanonicalSemanticRebuildPolicy maps the legacy zero value to manual and
// rejects unknown policies. The endpoint-specific boundary is checked by
// ValidateSemanticSettings once the whole configuration is available.
func CanonicalSemanticRebuildPolicy(policy SemanticRebuildPolicy) (SemanticRebuildPolicy, error) {
	switch policy {
	case "", SemanticRebuildManual:
		return SemanticRebuildManual, nil
	case SemanticRebuildAILocal:
		return SemanticRebuildAILocal, nil
	case SemanticRebuildAIRemote:
		return SemanticRebuildAIRemote, nil
	default:
		return "", fmt.Errorf("semantic rebuild_policy=%q 不受支持", policy)
	}
}

// SemanticSettings 是 canonical repo 对应的本机私有配置，不属于知识正本，
// 不进入 Git 或 bundle。Enabled 只有经本机 CLI 显式写入才可能为 true。
type SemanticSettings struct {
	Schema        int                   `json:"schema"`
	Enabled       bool                  `json:"enabled"`
	Endpoint      string                `json:"endpoint"`
	Model         string                `json:"model"`
	Dimensions    int                   `json:"dimensions,omitempty"`
	Revision      string                `json:"revision,omitempty"`
	QueryProfile  semantic.QueryProfile `json:"query_profile,omitempty"`
	RebuildPolicy SemanticRebuildPolicy `json:"rebuild_policy,omitempty"`
	TopK          int                   `json:"top_k"`
	MinScore      float64               `json:"min_score"`
	MaxVectorMiB  int                   `json:"max_vector_mib"`
	TimeoutSec    int                   `json:"timeout_seconds"`
}

// DefaultSemanticSettings 返回禁用态默认值。
func DefaultSemanticSettings() SemanticSettings {
	return SemanticSettings{
		Schema: semanticSettingsSchema, TopK: semanticDefaultTopK,
		MinScore: semanticDefaultMin, MaxVectorMiB: semanticDefaultMaxMiB,
		TimeoutSec: semanticDefaultTimeout, QueryProfile: semantic.QueryProfilePlain,
		RebuildPolicy: SemanticRebuildManual,
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
	// Provider configuration and every network-bearing semantic operation share
	// the cross-process semantic lock. Therefore configure/enable/disable cannot
	// return successfully while a query or rebuild is still about to emit a
	// request under the previous authorization.
	release, err := s.AcquireSemanticConfigWriteLock()
	if err != nil {
		return fmt.Errorf("semantic 配置暂不能修改: %w", err)
	}
	defer release()
	return s.WriteSemanticConfig(data)
}

func normalizeSemanticSettings(cfg *SemanticSettings) {
	cfg.Endpoint = strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.Revision = strings.TrimSpace(cfg.Revision)
	if cfg.QueryProfile == "" {
		// Schema 1 predates query profiles. Its wire behavior was plain.
		cfg.QueryProfile = semantic.QueryProfilePlain
	}
	if cfg.RebuildPolicy == "" {
		// Schema 1 predates MCP-triggered synchronization authorization.
		cfg.RebuildPolicy = SemanticRebuildManual
	}
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
	if _, err := semantic.CanonicalQueryProfile(cfg.QueryProfile); err != nil {
		return fmt.Errorf("semantic query_profile=%q 不受支持", cfg.QueryProfile)
	}
	policy, err := CanonicalSemanticRebuildPolicy(cfg.RebuildPolicy)
	if err != nil {
		return err
	}
	if !cfg.Enabled && cfg.Endpoint == "" && cfg.Model == "" {
		if policy != SemanticRebuildManual {
			return fmt.Errorf("semantic rebuild_policy=%s 需要先配置 endpoint", policy)
		}
		return nil
	}
	if cfg.Endpoint == "" || cfg.Model == "" {
		return fmt.Errorf("semantic endpoint 与 model 必须同时配置")
	}
	if len(cfg.Model) > 256 || hasControl(cfg.Model) {
		return fmt.Errorf("semantic model 非法或过长")
	}
	if err := validateSemanticEndpoint(cfg.Endpoint); err != nil {
		return err
	}
	switch policy {
	case SemanticRebuildManual:
		return nil
	case SemanticRebuildAILocal:
		if !semanticEndpointLoopback(cfg.Endpoint) {
			return fmt.Errorf("semantic rebuild_policy=ai-local 仅允许 loopback endpoint")
		}
		return nil
	case SemanticRebuildAIRemote:
		u, _ := url.Parse(cfg.Endpoint) // endpoint 已由 validateSemanticEndpoint 校验。
		if u.Scheme != "https" || semanticEndpointLoopback(cfg.Endpoint) {
			return fmt.Errorf("semantic rebuild_policy=ai-remote 仅允许 HTTPS 非 loopback endpoint")
		}
		return nil
	default:
		return fmt.Errorf("semantic rebuild_policy=%q 不受支持", policy)
	}
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

// semanticEndpointOrigin returns a canonical scheme://host[:port] audience.
// The endpoint itself must already satisfy the same remote/loopback policy as
// persisted settings; default ports are omitted so equivalent spellings bind
// to one credential audience.
func semanticEndpointOrigin(endpoint string) (string, error) {
	if err := validateSemanticEndpoint(strings.TrimRight(strings.TrimSpace(endpoint), "/")); err != nil {
		return "", err
	}
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", fmt.Errorf("semantic endpoint 不是完整 URL")
	}
	scheme := strings.ToLower(u.Scheme)
	hostname := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	host := hostname
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	return scheme + "://" + host, nil
}

// canonicalSemanticCredentialOrigin accepts only an origin (an optional root
// slash is harmless), never a path, query, fragment, or userinfo. It is kept
// outside repository settings: the environment owner, not repository data,
// selects which remote audience may receive the process credential.
func canonicalSemanticCredentialOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" ||
		(u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("%s 必须是无路径、userinfo、query、fragment 的完整 origin", SemanticAPIOriginEnv)
	}
	return semanticEndpointOrigin(u.Scheme + "://" + u.Host)
}

// SemanticSettingsFingerprint 标识向量空间与预处理契约；API key 不参与。
func SemanticSettingsFingerprint(cfg SemanticSettings) string {
	normalizeSemanticSettings(&cfg)
	fields := []string{
		"iknowledge-semantic-settings-v1", cfg.Endpoint, cfg.Model,
		strconv.Itoa(cfg.Dimensions), cfg.Revision, string(cfg.QueryProfile),
		"typed-cards-current-risk-history-v1", semanticSourcePreprocessVersion,
		"redact-v1", semanticDocumentPreprocessVersion, "distinct-node-topk-v1", "l2-v1",
		"normalized-canary-fingerprint-v2",
	}
	sum := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
	return "v1:" + hex.EncodeToString(sum[:])
}
