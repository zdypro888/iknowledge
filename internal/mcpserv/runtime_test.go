package mcpserv

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zdypro888/iknowledge/internal/engine"
	"github.com/zdypro888/iknowledge/internal/store"
)

func TestRuntimeIdentityAndGracefulShutdownEndpoint(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(st)
	if _, err := e.Init(engine.InitOptions{}); err != nil {
		t.Fatal(err)
	}
	shutdown := make(chan struct{}, 1)
	srv := New(e)
	identity, err := st.EnsureLocalIdentity()
	if err != nil {
		t.Fatal(err)
	}
	srv.LocalIdentity = identity
	srv.Shutdown = func() { shutdown <- struct{}{} }
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/runtime")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Schema   int    `json:"schema"`
		RepoRoot string `json:"repo_root"`
		Build    struct {
			Version          string `json:"version"`
			ExecutableSHA256 string `json:"executable_sha256"`
		} `json:"build"`
		StartedAt string `json:"started_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		_ = resp.Body.Close()
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || status.Schema != 1 || status.RepoRoot != repo ||
		status.Build.Version == "" || status.Build.ExecutableSHA256 == "" || status.StartedAt == "" {
		t.Fatalf("runtime status=%d %+v", resp.StatusCode, status)
	}

	localSession, err := st.AcquireLocalAuthSession(context.Background(), ts.URL, "/runtime/shutdown", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/runtime/shutdown", nil)
	req.Header.Set("Authorization", store.LocalSessionAuthScheme+" "+localSession.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("shutdown status=%d", resp.StatusCode)
	}
	select {
	case <-shutdown:
	case <-time.After(time.Second):
		t.Fatal("shutdown callback was not invoked")
	}
}
