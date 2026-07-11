package store

import (
	"fmt"
	"hash/fnv"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/zdypro888/iknowledge/internal/model"
)

// Config 是 .knowledge/config.yaml(impl §1/§5):端口与 include/exclude 覆盖。
type Config struct {
	Schema  int      `yaml:"schema"`
	Port    int      `yaml:"port"`
	Include []string `yaml:"include,omitempty"` // 非空时仅索引匹配路径(path.Match 语法,正斜杠相对路径)
	Exclude []string `yaml:"exclude,omitempty"` // 追加排除(在 vendor/testdata/.knowledge/生成代码之上)

	// Extensions 通用文件级覆盖白名单(2026-07-04 多语言 T0,缺省关):
	// 列出的扩展名(如 [".proto", ".sql"])以文件粒度入库——账本/经验/hook/
	// 腐烂检测(内容哈希)全可用,无符号粒度与调用图;已有专职解析器的扩展名
	// (.go/.py)忽略。改动本字段需重启 serve(Registry 启动期构建)。
	Extensions []string `yaml:"extensions,omitempty"`

	// 侦查模式(impl §7.5,轮 22 定案委派为主、自派为备;2026-07-04 备模式实装):
	// Scout 空/"delegate" = 委派(kb_investigate 秒回简报,宿主子代理执行);
	// "self" = 自派(服务端 PTY 驱动 ScoutCommand,阻塞等交卷,面向无子代理宿主)。
	Scout string `yaml:"scout,omitempty"`
	// ScoutCommand 自派命令模板,{mcp} 占位 MCP 配置文件路径;缺省
	// `claude --mcp-config {mcp} --strict-mcp-config --allowedTools "mcp__knowledge__*"`。
	ScoutCommand string `yaml:"scout_command,omitempty"`
	// ScoutTimeoutSec 自派交卷等待上限秒数,缺省 300(job TTL 30 分钟另计:
	// 超时后迟到交卷仍落库,kb_status 可见——迟到不白费)。
	ScoutTimeoutSec int `yaml:"scout_timeout_seconds,omitempty"`
}

// DerivePort 端口分配定案(impl §1):18000 + fnv32a(repo 绝对路径) % 2000。
func DerivePort(repoAbs string) int {
	h := fnv.New32a()
	h.Write([]byte(repoAbs))
	return 18000 + int(h.Sum32()%2000)
}

func (s *Store) configPath() string { return filepath.Join(s.dir, "config.yaml") }

// LoadConfig 读配置;文件不存在返回 (nil, nil)。
func (s *Store) LoadConfig() (*Config, error) {
	data, err := s.readKnowledgeFile(s.configPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读 config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("store: 解析 config: %w", err)
	}
	if err := ValidateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("store: 非法 config: %w", err)
	}
	return &cfg, nil
}

// ValidateConfig 是运行时与 bundle import 共用的 fail-closed 配置校验。
func ValidateConfig(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config 为空")
	}
	if cfg.Schema != model.SchemaVersion {
		return fmt.Errorf("schema=%d，当前仅支持 %d", cfg.Schema, model.SchemaVersion)
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return fmt.Errorf("port=%d 越界(1..65535)", cfg.Port)
	}
	if cfg.Scout != "" && cfg.Scout != "delegate" && cfg.Scout != "self" {
		return fmt.Errorf("scout=%q 非法(仅 delegate|self)", cfg.Scout)
	}
	if cfg.ScoutTimeoutSec < 0 {
		return fmt.Errorf("scout_timeout_seconds 不得为负")
	}
	for _, group := range []struct {
		kind     string
		patterns []string
	}{{"include", cfg.Include}, {"exclude", cfg.Exclude}} {
		for _, pattern := range group.patterns {
			if pattern == "" || strings.HasPrefix(pattern, "/") || strings.ContainsAny(pattern, "\\\x00") {
				return fmt.Errorf("%s pattern %q 非法", group.kind, pattern)
			}
			for segment := range strings.SplitSeq(strings.TrimSuffix(pattern, "/"), "/") {
				if segment == "." || segment == ".." {
					return fmt.Errorf("%s pattern %q 含非法路径段", group.kind, pattern)
				}
			}
			if _, err := path.Match(strings.TrimSuffix(pattern, "/"), "probe"); err != nil {
				return fmt.Errorf("%s pattern %q: %w", group.kind, pattern, err)
			}
		}
	}
	for _, ext := range cfg.Extensions {
		name := strings.TrimPrefix(ext, ".")
		if name == "" || len(ext) > 32 || strings.ContainsAny(ext, "/\\\x00") || strings.Contains(name, ".") {
			return fmt.Errorf("extension %q 非法(应为 ext 或 .ext)", ext)
		}
	}
	return nil
}

// EnsureConfig 幂等写配置:不存在才生成(用户手改的 config 永不被 init 覆盖),
// 返回生效配置。
func (s *Store) EnsureConfig() (*Config, error) {
	if cfg, err := s.LoadConfig(); err != nil || cfg != nil {
		return cfg, err
	}
	cfg := &Config{Schema: 1, Port: DerivePort(s.repo)}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("store: 编码 config: %w", err)
	}
	if err := s.atomicWrite(s.configPath(), data); err != nil {
		return nil, err
	}
	return cfg, nil
}
