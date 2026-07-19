package engine

import (
	"bytes"
	"os"
	"testing"

	"github.com/zdypro888/iknowledge/internal/semantic"
	"github.com/zdypro888/iknowledge/internal/store"
)

func semanticStore(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSemanticSettingsRoundTrip(t *testing.T) {
	s := semanticStore(t)
	cfg := DefaultSemanticSettings()
	cfg.Enabled = true
	cfg.Endpoint = "http://127.0.0.1:11434/v1/"
	cfg.Model = "qwen3-embedding:0.6b"
	cfg.Dimensions = 1024
	cfg.Revision = "ollama-local-v1"
	cfg.QueryProfile = semantic.QueryProfileQwen3CodeV1
	cfg.RebuildPolicy = SemanticRebuildAILocal
	if err := SaveSemanticSettings(s, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSemanticSettings(s)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Endpoint != "http://127.0.0.1:11434/v1" ||
		got.Model != cfg.Model || got.Dimensions != 1024 || got.TopK != semanticDefaultTopK ||
		got.QueryProfile != semantic.QueryProfileQwen3CodeV1 {
		t.Fatalf("roundtrip=%+v", got)
	}
	if got.RebuildPolicy != SemanticRebuildAILocal {
		t.Fatalf("roundtrip rebuild_policy=%q", got.RebuildPolicy)
	}
}

func TestSemanticSettingsLegacyMissingQueryProfileUsesPlain(t *testing.T) {
	s := semanticStore(t)
	raw := `{"schema":1,"enabled":true,"endpoint":"http://127.0.0.1:11434/v1","model":"legacy-model","top_k":20,"min_score":0.35,"max_vector_mib":512,"timeout_seconds":30}`
	if err := s.WriteSemanticConfig([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadSemanticSettings(s)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QueryProfile != semantic.QueryProfilePlain {
		t.Fatalf("legacy query_profile=%q, want plain", cfg.QueryProfile)
	}
	if cfg.RebuildPolicy != SemanticRebuildManual {
		t.Fatalf("legacy rebuild_policy=%q, want manual", cfg.RebuildPolicy)
	}
}

func TestSemanticSettingsMissingIsDisabled(t *testing.T) {
	cfg, err := LoadSemanticSettings(semanticStore(t))
	if err != nil || cfg.Enabled {
		t.Fatalf("missing config=%+v err=%v", cfg, err)
	}
}

func TestSemanticSettingsPreservesExplicitZeroMinScore(t *testing.T) {
	s := semanticStore(t)
	cfg := DefaultSemanticSettings()
	cfg.Enabled = true
	cfg.Endpoint = "http://127.0.0.1:11434/v1"
	cfg.Model = "embed"
	cfg.MinScore = 0
	if err := SaveSemanticSettings(s, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSemanticSettings(s)
	if err != nil {
		t.Fatal(err)
	}
	if got.MinScore != 0 {
		t.Fatalf("min_score=%g, want explicit 0", got.MinScore)
	}
}

func TestSemanticSettingsRejectsExplicitZeroResourceBounds(t *testing.T) {
	base := DefaultSemanticSettings()
	for name, mutate := range map[string]func(*SemanticSettings){
		"schema":         func(c *SemanticSettings) { c.Schema = 0 },
		"top_k":          func(c *SemanticSettings) { c.TopK = 0 },
		"max_vector_mib": func(c *SemanticSettings) { c.MaxVectorMiB = 0 },
		"timeout":        func(c *SemanticSettings) { c.TimeoutSec = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			if err := ValidateSemanticSettings(cfg); err == nil {
				t.Fatalf("显式零值应被拒绝: %+v", cfg)
			}
		})
	}
}

func TestSemanticSettingsRejectsUnknownQueryProfile(t *testing.T) {
	cfg := DefaultSemanticSettings()
	cfg.QueryProfile = semantic.QueryProfile("future-profile")
	if err := ValidateSemanticSettings(cfg); err == nil {
		t.Fatal("unknown query profile succeeded")
	}
}

func TestSemanticSettingsRebuildPolicyEndpointBoundary(t *testing.T) {
	base := DefaultSemanticSettings()
	base.Enabled = true
	base.Model = "embed"
	tests := []struct {
		name     string
		policy   SemanticRebuildPolicy
		endpoint string
		wantErr  bool
	}{
		{name: "manual local", policy: SemanticRebuildManual, endpoint: "http://127.0.0.1:11434/v1"},
		{name: "manual remote", policy: SemanticRebuildManual, endpoint: "https://embed.example.com/v1"},
		{name: "ai local loopback http", policy: SemanticRebuildAILocal, endpoint: "http://localhost:11434/v1"},
		{name: "ai local loopback https", policy: SemanticRebuildAILocal, endpoint: "https://[::1]:11434/v1"},
		{name: "ai local rejects remote", policy: SemanticRebuildAILocal, endpoint: "https://embed.example.com/v1", wantErr: true},
		{name: "ai remote https", policy: SemanticRebuildAIRemote, endpoint: "https://embed.example.com/v1"},
		{name: "ai remote rejects loopback", policy: SemanticRebuildAIRemote, endpoint: "https://127.0.0.1:11434/v1", wantErr: true},
		{name: "ai remote rejects http", policy: SemanticRebuildAIRemote, endpoint: "http://127.0.0.1:11434/v1", wantErr: true},
		{name: "unknown", policy: SemanticRebuildPolicy("future"), endpoint: "https://embed.example.com/v1", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			cfg.RebuildPolicy = test.policy
			cfg.Endpoint = test.endpoint
			err := ValidateSemanticSettings(cfg)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateSemanticSettings() error=%v, wantErr=%v", err, test.wantErr)
			}
		})
	}

	unconfigured := DefaultSemanticSettings()
	unconfigured.RebuildPolicy = SemanticRebuildAILocal
	if err := ValidateSemanticSettings(unconfigured); err == nil {
		t.Fatal("ai-local without endpoint succeeded")
	}
}

func TestSemanticEndpointSecurity(t *testing.T) {
	base := DefaultSemanticSettings()
	base.Enabled = true
	base.Model = "embed"
	tests := []struct {
		endpoint string
		ok       bool
	}{
		{"http://127.0.0.1:11434/v1", true},
		{"http://[::1]:11434/v1", true},
		{"http://localhost:11434/v1", true},
		{"https://embed.example.com/v1", true},
		{"http://embed.example.com/v1", false},
		{"https://user:pass@embed.example.com/v1", false},
		{"file:///tmp/model", false},
		{"https://embed.example.com/v1?key=secret", false},
		{"https://embed.example.com/v1?", false},
		{"https://embed.example.com/a//b", false},
		{"https://embed.example.com/a/../b", false},
		{"https://embed.example.com/%76%31", false},
	}
	for _, tt := range tests {
		cfg := base
		cfg.Endpoint = tt.endpoint
		err := ValidateSemanticSettings(cfg)
		if (err == nil) != tt.ok {
			t.Errorf("endpoint=%q err=%v ok=%v", tt.endpoint, err, tt.ok)
		}
	}
}

func TestSemanticSettingsRejectUnknownAndTrailingJSON(t *testing.T) {
	for _, raw := range []string{
		`{"schema":1,"enabled":false,"top_k":20,"min_score":0.35,"max_vector_mib":512,"timeout_seconds":30,"surprise":true}`,
		`{"schema":1,"enabled":false,"top_k":20,"min_score":0.35,"max_vector_mib":512,"timeout_seconds":30} {}`,
	} {
		s := semanticStore(t)
		if err := s.WriteSemanticConfig([]byte(raw)); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSemanticSettings(s); err == nil {
			t.Fatalf("应拒绝配置: %s", raw)
		}
	}
}

func TestSemanticFingerprintCoversVectorSpace(t *testing.T) {
	cfg := DefaultSemanticSettings()
	cfg.Enabled = true
	cfg.Endpoint = "https://embed.example.com/v1"
	cfg.Model = "model-a"
	base := SemanticSettingsFingerprint(cfg)
	mutations := []func(*SemanticSettings){
		func(c *SemanticSettings) { c.Endpoint = "https://other.example.com/v1" },
		func(c *SemanticSettings) { c.Model = "model-b" },
		func(c *SemanticSettings) { c.Dimensions = 768 },
		func(c *SemanticSettings) { c.Revision = "r2" },
		func(c *SemanticSettings) { c.QueryProfile = semantic.QueryProfileQwen3CodeV1 },
	}
	for i, mutate := range mutations {
		copy := cfg
		mutate(&copy)
		if got := SemanticSettingsFingerprint(copy); got == base {
			t.Errorf("mutation %d 未改变 fingerprint", i)
		}
	}
}

func TestSemanticAPIKeyNeverPersistedBySettings(t *testing.T) {
	s := semanticStore(t)
	t.Setenv(SemanticAPIKeyEnv, "sk-test-secret-value")
	cfg := DefaultSemanticSettings()
	cfg.Enabled = true
	cfg.Endpoint = "https://embed.example.com/v1"
	cfg.Model = "embed"
	if err := SaveSemanticSettings(s, cfg); err != nil {
		t.Fatal(err)
	}
	path, err := s.SemanticConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || bytes.Contains(data, []byte("sk-test-secret-value")) {
		t.Fatalf("semantic 配置泄露 API key: %q", data)
	}
}

func TestCanonicalSemanticCredentialOrigin(t *testing.T) {
	for input, want := range map[string]string{
		"https://EMBED.example.com:443/": "https://embed.example.com",
		"https://embed.example.com:8443": "https://embed.example.com:8443",
		"https://[::1]:443":              "https://[::1]",
	} {
		got, err := canonicalSemanticCredentialOrigin(input)
		if err != nil || got != want {
			t.Errorf("canonicalSemanticCredentialOrigin(%q)=(%q,%v), want %q", input, got, err, want)
		}
	}
	for _, input := range []string{
		"", "embed.example.com", "https://user@embed.example.com",
		"https://embed.example.com/v1", "https://embed.example.com?tenant=b",
		"https://embed.example.com#fragment",
	} {
		if got, err := canonicalSemanticCredentialOrigin(input); err == nil {
			t.Errorf("invalid credential origin %q accepted as %q", input, got)
		}
	}
}
