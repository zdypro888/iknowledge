package store

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/model"
)

func symlinkOrSkip(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Windows runner 无创建 symlink 权限: %v", err)
		}
		t.Fatal(err)
	}
}

func TestKnowledgeSymlinkBoundary(t *testing.T) {
	t.Run("knowledge 根链接在 Open 时拒绝", func(t *testing.T) {
		repo := t.TempDir()
		outside := t.TempDir()
		symlinkOrSkip(t, outside, filepath.Join(repo, KnowledgeDir))
		_, err := Open(repo)
		if !errors.Is(err, ErrSymlinkPath) {
			t.Fatalf("Open err=%v, want ErrSymlinkPath", err)
		}
	})

	tests := []struct {
		name string
		run  func(t *testing.T, s *Store, outside string) error
	}{
		{
			name: "tree 父目录链接挡住原子分片写",
			run: func(t *testing.T, s *Store, outside string) error {
				if err := os.RemoveAll(filepath.Join(s.Dir(), "tree")); err != nil {
					t.Fatal(err)
				}
				symlinkOrSkip(t, outside, filepath.Join(s.Dir(), "tree"))
				return s.SaveShard(s.ShardPathFor("escape.go"), sampleShard(), nil)
			},
		},
		{
			name: "journal 最终文件链接挡住追加",
			run: func(t *testing.T, s *Store, outside string) error {
				target := filepath.Join(outside, "journal-target")
				if err := os.WriteFile(target, []byte("sentinel"), 0o644); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(s.Dir(), "journal", "2026-07.jsonl")
				symlinkOrSkip(t, target, link)
				err := s.AppendChange(model.Change{ID: "chg_x", At: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)})
				if got, readErr := os.ReadFile(target); readErr != nil || string(got) != "sentinel" {
					t.Fatalf("仓外 journal 被改写: %q err=%v", got, readErr)
				}
				return err
			},
		},
		{
			name: "local 父目录链接挡住追加日志",
			run: func(t *testing.T, s *Store, outside string) error {
				if err := os.RemoveAll(filepath.Join(s.Dir(), "local")); err != nil {
					t.Fatal(err)
				}
				symlinkOrSkip(t, outside, filepath.Join(s.Dir(), "local"))
				f, err := s.OpenKnowledgeLog("local/serve.log", 0o644)
				if f != nil {
					_ = f.Close()
				}
				return err
			},
		},
		{
			name: "flow 最终文件链接挡住保存",
			run: func(t *testing.T, s *Store, outside string) error {
				target := filepath.Join(outside, "flow")
				if err := os.WriteFile(target, []byte("sentinel"), 0o644); err != nil {
					t.Fatal(err)
				}
				symlinkOrSkip(t, target, s.FlowPathFor("flow:escape"))
				return s.SaveFlow(model.Flow{ID: "flow:escape", Title: "escape"})
			},
		},
		{
			name: "WIP 最终文件链接挡住保存",
			run: func(t *testing.T, s *Store, outside string) error {
				target := filepath.Join(outside, "wip")
				if err := os.WriteFile(target, []byte("sentinel"), 0o644); err != nil {
					t.Fatal(err)
				}
				symlinkOrSkip(t, target, s.wipPath("owner"))
				return s.SaveWIP(model.WIP{Owner: "owner", Task: "escape"})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			outside := t.TempDir()
			if err := tt.run(t, s, outside); !errors.Is(err, ErrSymlinkPath) {
				t.Fatalf("err=%v, want ErrSymlinkPath", err)
			}
			if entries, err := os.ReadDir(outside); err != nil {
				t.Fatal(err)
			} else if tt.name == "tree 父目录链接挡住原子分片写" && len(entries) != 0 {
				t.Fatalf("仓外产生文件: %v", entries)
			}
		})
	}
}

func TestEnsureLayoutRejectsExistingSymlink(t *testing.T) {
	repo := t.TempDir()
	s, err := Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, KnowledgeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, t.TempDir(), filepath.Join(s.Dir(), "tree"))
	if err := s.EnsureLayout(); !errors.Is(err, ErrSymlinkPath) {
		t.Fatalf("EnsureLayout err=%v, want ErrSymlinkPath", err)
	}
}

func TestLoadAuthTokenRepairsPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows POSIX permission bits do not map to ACLs")
	}
	t.Setenv(stateHomeEnv, t.TempDir())
	s := newStore(t)
	path, err := s.authTokenPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateStateDir(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	token := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := os.WriteFile(path, []byte(token+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LoadAuthToken(); err != nil || got != token {
		t.Fatalf("LoadAuthToken=%q/%v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode=%o, want 600", info.Mode().Perm())
	}
}

func TestLoadAuthTokenRejectsCorruptExistingFile(t *testing.T) {
	for _, content := range []string{"", "short", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"} {
		t.Run(content, func(t *testing.T) {
			t.Setenv(stateHomeEnv, t.TempDir())
			s := newStore(t)
			path, err := s.authTokenPath()
			if err != nil {
				t.Fatal(err)
			}
			if err := ensurePrivateStateDir(filepath.Dir(path)); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.LoadAuthToken(); err == nil {
				t.Fatal("已存在但损坏的 token 必须 fail closed")
			}
		})
	}
}

func TestLegacyAuthTokenRotatesOutsideRepo(t *testing.T) {
	t.Setenv(stateHomeEnv, t.TempDir())
	s := newStore(t)
	legacy := filepath.Join(s.Dir(), filepath.FromSlash(legacyAuthTokenRel))
	known := strings.Repeat("a", 64)
	if err := os.WriteFile(legacy, []byte(known+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	if got == known || !ValidLocalAuthValue(got) {
		t.Fatalf("legacy attacker-known token 未安全轮换: %q", got)
	}
	if _, err := os.Lstat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy token 未删除: %v", err)
	}
	path, err := s.authTokenPath()
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(path, s.Dir()+string(filepath.Separator)) {
		t.Fatalf("根 token 仍在仓库内: %s", path)
	}
}

func TestScoutTrustNeverMigratesRepoMarkerImplicitly(t *testing.T) {
	t.Setenv(stateHomeEnv, t.TempDir())
	s := newStore(t)
	legacy := filepath.Join(s.Dir(), filepath.FromSlash(legacyScoutTrustRel))
	if err := os.WriteFile(legacy, []byte("attacker-known\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LoadScoutTrust(); !os.IsNotExist(err) || got != "" {
		t.Fatalf("仓内 marker 不得隐式迁移: got=%q err=%v", got, err)
	}
	if _, err := os.Lstat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy marker 未删除: %v", err)
	}
	if err := s.WriteScoutTrust("v1:trusted"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LoadScoutTrust(); err != nil || got != "v1:trusted" {
		t.Fatalf("外部 trust 往返失败: %q/%v", got, err)
	}
}

func TestEnsureLocalIdentityConcurrentFirstCreate(t *testing.T) {
	t.Setenv(stateHomeEnv, t.TempDir())
	s := newStore(t)
	const workers = 32
	start := make(chan struct{})
	values := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			value, err := s.EnsureLocalIdentity()
			if err != nil {
				errs <- err
				return
			}
			values <- value
		}()
	}
	close(start)
	wg.Wait()
	close(values)
	close(errs)
	for err := range errs {
		t.Errorf("EnsureLocalIdentity 并发失败: %v", err)
	}
	var first string
	for value := range values {
		if first == "" {
			first = value
		}
		if value != first {
			t.Errorf("并发首次创建返回了不同身份: %q != %q", value, first)
		}
	}
	if !ValidLocalAuthValue(first) {
		t.Fatalf("生成身份非法: %q", first)
	}
}

func TestPrivateStateRejectsSymlinkBelowTrustedRoot(t *testing.T) {
	stateHome := t.TempDir()
	outside := t.TempDir()
	symlinkOrSkip(t, outside, filepath.Join(stateHome, "repos"))
	t.Setenv(stateHomeEnv, stateHome)
	s := newStore(t)
	if _, err := s.EnsureLocalIdentity(); err == nil || !strings.Contains(err.Error(), "真目录") {
		t.Fatalf("repos symlink 必须拒绝: %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("私有状态越界写入链接目标: %v", entries)
	}
}

func TestPrivateStateDoesNotChmodTrustedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 权限由 ACL 表达")
	}
	stateHome := t.TempDir()
	if err := os.Chmod(stateHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(stateHomeEnv, stateHome)
	s := newStore(t)
	if _, err := s.EnsureLocalIdentity(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(stateHome)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("不得擅自 chmod 用户选择的根: mode=%o", info.Mode().Perm())
	}
}

func TestSemanticConfigLivesOutsideRepository(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv(stateHomeEnv, stateHome)
	s := newStore(t)
	data := []byte("{\"enabled\":true,\"endpoint\":\"http://127.0.0.1:11434/v1\"}\n")
	if err := s.WriteSemanticConfig(data); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSemanticConfig()
	if err != nil || string(got) != string(data) {
		t.Fatalf("semantic 配置往返失败: %q/%v", got, err)
	}
	large := []byte(strings.Repeat("x", 8<<10))
	if err := s.WriteSemanticConfig(large); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LoadSemanticConfig(); err != nil || string(got) != string(large) {
		t.Fatalf("超过通用私有态 4KiB 的 semantic 配置应按专用上限往返: len=%d err=%v", len(got), err)
	}
	path, err := s.SemanticConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(filepath.Clean(path), filepath.Clean(s.Dir())+string(filepath.Separator)) {
		t.Fatalf("semantic provider 配置不得进入仓库: %s", path)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("semantic 配置权限=%o, want 600", info.Mode().Perm())
		}
	}
	if err := s.RemoveSemanticConfig(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadSemanticConfig(); !os.IsNotExist(err) {
		t.Fatalf("删除后仍可读取 semantic 配置: %v", err)
	}
}
