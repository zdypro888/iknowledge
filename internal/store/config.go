package store

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config 是 .knowledge/config.yaml(impl §1/§5):端口与 include/exclude 覆盖。
type Config struct {
	Schema  int      `yaml:"schema"`
	Port    int      `yaml:"port"`
	Include []string `yaml:"include,omitempty"` // 非空时仅索引匹配路径(path.Match 语法,正斜杠相对路径)
	Exclude []string `yaml:"exclude,omitempty"` // 追加排除(在 vendor/testdata/.knowledge/生成代码之上)

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
	data, err := os.ReadFile(s.configPath())
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
	return &cfg, nil
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
	if err := atomicWrite(s.configPath(), data); err != nil {
		return nil, err
	}
	return cfg, nil
}
