package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/mcpserv"
	"github.com/zdypro888/iknowledge/internal/store"
)

func TestEnsureCurrentServeGenerationKeepsMatchingDaemon(t *testing.T) {
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})
	identity, err := e.Store.EnsureLocalIdentity()
	if err != nil {
		t.Fatal(err)
	}
	srv := mcpserv.New(e)
	srv.LocalIdentity = identity
	var called atomic.Bool
	srv.Shutdown = func() { called.Store(true) }
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	current, repos, err := ensureCurrentServeGeneration(e.Store, ts.URL)
	if err != nil || !current || called.Load() {
		t.Fatalf("matching daemon current=%v shutdown=%v err=%v", current, called.Load(), err)
	}
	if len(repos) != 1 || !sameRepoPath(repos[0], repo) {
		t.Fatalf("runtime repos=%v, want %s", repos, repo)
	}
}

func TestEnsureCurrentServeGenerationGracefullyReplacesOldDaemon(t *testing.T) {
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})
	identity, err := e.Store.EnsureLocalIdentity()
	if err != nil {
		t.Fatal(err)
	}
	srv := mcpserv.New(e)
	srv.LocalIdentity = identity
	srv.Build.Version = "old-test-build"
	srv.Build.ExecutableSHA256 = "deadbeef"
	var ts *httptest.Server
	var once sync.Once
	srv.Shutdown = func() { once.Do(ts.Close) }
	ts = httptest.NewServer(srv.Handler())

	current, repos, err := ensureCurrentServeGeneration(e.Store, ts.URL)
	if err != nil || current {
		t.Fatalf("old daemon current=%v err=%v", current, err)
	}
	if len(repos) != 1 || !sameRepoPath(repos[0], repo) {
		t.Fatalf("retired repos=%v, want %s", repos, repo)
	}
}

func TestRuntimeRolloverPreservesMultiRepoGroup(t *testing.T) {
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	repoA := setupGitRepo(t)
	repoB := setupGitRepo(t)
	e, _ := initRepo(t, repoA, engine.InitOptions{})
	initRepo(t, repoB, engine.InitOptions{})
	identity, err := e.Store.EnsureLocalIdentity()
	if err != nil {
		t.Fatal(err)
	}
	srv := mcpserv.New(e)
	srv.LocalIdentity = identity
	srv.RuntimeRepos = []string{repoA, repoB}
	srv.Build.Version = "old-test-build"
	srv.Build.ExecutableSHA256 = "deadbeef"
	var ts *httptest.Server
	var once sync.Once
	srv.Shutdown = func() { once.Do(ts.Close) }
	ts = httptest.NewServer(srv.Handler())

	current, repos, err := ensureCurrentServeGeneration(e.Store, ts.URL)
	if err != nil || current {
		t.Fatalf("old multi-repo daemon current=%v err=%v", current, err)
	}
	if len(repos) != 2 || !sameRepoPath(repos[0], repoA) || !sameRepoPath(repos[1], repoB) {
		t.Fatalf("retired repo group=%v, want [%s %s]", repos, repoA, repoB)
	}
}

func TestEnsureCurrentServeGenerationClassifiesLegacyRuntimeScope(t *testing.T) {
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})
	identity, err := e.Store.EnsureLocalIdentity()
	if err != nil {
		t.Fatal(err)
	}
	srv := mcpserv.New(e)
	srv.LocalIdentity = identity
	currentHandler := srv.Handler()
	legacyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == store.LocalAuthChallengePath {
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Errorf("read challenge: %v", readErr)
				http.Error(w, "read challenge", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var challenge struct {
				Scope string `json:"scope"`
			}
			if json.Unmarshal(body, &challenge) == nil && challenge.Scope == "/runtime" {
				// This is the exact compatibility behavior of the daemon version
				// immediately before /runtime was added: /mcp/main authenticates,
				// while the new scope is rejected at challenge validation.
				http.Error(w, "invalid local auth request", http.StatusBadRequest)
				return
			}
		}
		currentHandler.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(legacyHandler)
	t.Cleanup(ts.Close)

	current, repos, err := ensureCurrentServeGeneration(e.Store, ts.URL)
	if current || repos != nil || !errors.Is(err, errRuntimeEndpointMissing) {
		t.Fatalf("legacy daemon current=%v repos=%v err=%v", current, repos, err)
	}
	if !strings.Contains(err.Error(), "不支持安全换代的旧版本") {
		t.Fatalf("legacy daemon did not receive actionable upgrade guidance: %v", err)
	}
}

func TestRuntimeScopeRejectionWithoutMainProofIsNotLegacy(t *testing.T) {
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// An unrelated listener can copy the old daemon's HTTP status, but it
		// cannot produce the /mcp/main server proof bound to this repository.
		http.Error(w, "invalid local auth request", http.StatusBadRequest)
	}))
	t.Cleanup(fake.Close)

	_, err := probeServeRuntime(e.Store, fake.URL)
	if err == nil || errors.Is(err, errRuntimeEndpointMissing) {
		t.Fatalf("unproven listener classified as legacy daemon: %v", err)
	}
	if !strings.Contains(err.Error(), "兼容身份验证失败") {
		t.Fatalf("missing fail-closed identity diagnostic: %v", err)
	}
}

func TestDeployDoctorCountsAuthenticatedRuntimeProbeFailure(t *testing.T) {
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})
	cfg, err := e.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Port))
	if err != nil {
		t.Skipf("configured deploy-doctor test port unavailable: %v", err)
	}
	fake := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not iknowledge", http.StatusTeapot)
	}))
	fake.Listener = ln
	fake.Start()
	t.Cleanup(fake.Close)

	// Make PATH diagnostics deterministic and deliberately omit ps so process
	// warnings cannot make this assertion pass accidentally.
	binDir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(binDir, "iknowledge")); err != nil {
		t.Skipf("cannot create deterministic PATH fixture: %v", err)
	}
	t.Setenv("PATH", binDir)

	text, warnings := deployDoctorText(e.Store)
	if !strings.Contains(text, "⚠ repo daemon: runtime probe unavailable") {
		t.Fatalf("missing runtime probe diagnostic:\n%s", text)
	}
	if warnings != 1 {
		t.Fatalf("runtime probe failure warnings=%d, want exactly 1:\n%s", warnings, text)
	}
}

func TestDoctorUsesLiveDaemonWhenWriterLockIsHeld(t *testing.T) {
	t.Setenv("IKNOWLEDGE_STATE_HOME", t.TempDir())
	repo := setupGitRepo(t)
	e, _ := initRepo(t, repo, engine.InitOptions{})
	identity, err := e.Store.EnsureLocalIdentity()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := e.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Port))
	if err != nil {
		t.Skipf("configured doctor test port unavailable: %v", err)
	}
	srv := mcpserv.New(e)
	srv.LocalIdentity = identity
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)

	release, err := e.Store.AcquireWriterLock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	var out bytes.Buffer
	if code := runDoctor([]string{"--repo", repo}, &out); code != 0 {
		t.Fatalf("doctor code=%d output=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "doctor: "+repo) || !strings.Contains(out.String(), "parser:") {
		t.Fatalf("doctor did not use live owner view:\n%s", out.String())
	}
}
