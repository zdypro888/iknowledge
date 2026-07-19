package main

import (
	"reflect"
	"testing"
)

func TestParseImportRemaps(t *testing.T) {
	got, err := parseImportRemaps([]string{"internal/auth=pkg/auth,api=v2/api", "internal/auth=pkg/auth"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"internal/auth": "pkg/auth", "api": "v2/api"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("remaps=%v want %v", got, want)
	}
	for _, specs := range [][]string{
		{"missing-equals"},
		{"=target"},
		{"source="},
		{"a=b=c"},
		{"a=b", "a=c"},
		{"a=b,"},
	} {
		if _, err := parseImportRemaps(specs); err == nil {
			t.Errorf("非法 remap 应拒绝: %v", specs)
		}
	}
}
