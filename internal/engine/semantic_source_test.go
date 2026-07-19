package engine

import (
	"context"
	"strings"
	"testing"
)

func TestSemanticDocumentsOnlyUseActiveSummaries(t *testing.T) {
	e, _ := newRepo(t, map[string]string{
		"auth.go": "package auth\n\nfunc Login() {}\n",
	})
	if _, err := e.Init(InitOptions{}); err != nil {
		t.Fatal(err)
	}
	node := "auth.go#Login"
	if _, err := e.Remember(RememberArgs{Node: node, Entries: []RememberEntry{
		{Kind: "summary", Text: "连续登录失败会触发锁定，token=sk-abcdefghijklmnopqrstuvwxyz123456"},
		{Kind: "pitfall", Text: "不要绕过锁定检查"},
	}}, "s", "test"); err != nil {
		t.Fatal(err)
	}
	docs, manifest, err := e.semanticSourceSnapshot(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := manifest.fingerprint
	if len(docs) != 1 {
		t.Fatalf("documents=%d, want only summary: %+v", len(docs), docs)
	}
	if docs[0].NodeID != node || docs[0].Kind != "summary" || !strings.HasPrefix(docs[0].RecordID, "summary:"+node+"#") {
		t.Fatalf("unexpected document: %+v", docs[0])
	}
	if strings.Contains(docs[0].Text, "sk-abcdefghijklmnopqrstuvwxyz123456") ||
		!strings.Contains(docs[0].Text, "[REDACTED:openai-key]") {
		t.Fatalf("embedding 文本未脱敏: %q", docs[0].Text)
	}
	if fingerprint == ([32]byte{}) || docs[0].SourceHash == ([32]byte{}) {
		t.Fatal("fingerprint/source hash 不得为空")
	}

	// 普通 pitfall 不属于首版向量源，新增它不得改变语义源 fingerprint。
	if _, err := e.Remember(RememberArgs{Node: node, Entries: []RememberEntry{{
		Kind: "pitfall", Text: "另一个排障坑",
	}}}, "s", "test"); err != nil {
		t.Fatal(err)
	}
	after, manifestAfter, err := e.semanticSourceSnapshot(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	fingerprintAfter := manifestAfter.fingerprint
	if len(after) != 1 || fingerprintAfter != fingerprint {
		t.Fatalf("非摘要改变了 semantic snapshot: docs=%d before=%x after=%x", len(after), fingerprint, fingerprintAfter)
	}
}

func TestSemanticDocumentRedactsFinalNodePrefix(t *testing.T) {
	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	doc := makeSemanticDocument("summary:x#e", secret+".go#Load", "summary", "安全摘要")
	if strings.Contains(doc.Text, secret) || !strings.Contains(doc.Text, "[REDACTED:openai-key]") {
		t.Fatalf("最终 embedding 文本的 node ID 未脱敏: %q", doc.Text)
	}
	if len(doc.Text) > semanticMaxDocumentBytes {
		t.Fatalf("embedding 文本=%d bytes, 超过 %d", len(doc.Text), semanticMaxDocumentBytes)
	}
}

func TestCompactSemanticTextKeepsHeadAndTail(t *testing.T) {
	input := "HEAD-" + strings.Repeat("中", 100) + "-TAIL"
	got := compactSemanticText(input, 40)
	if len([]rune(got)) > 40 || !strings.HasPrefix(got, "HEAD-") || !strings.HasSuffix(got, "-TAIL") {
		t.Fatalf("compact=%q runes=%d", got, len([]rune(got)))
	}
}
