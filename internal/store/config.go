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
