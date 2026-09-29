package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")

	k1, err := loadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(k1) < 16 {
		t.Fatalf("key too short: %q", k1)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, want 600", st.Mode().Perm())
	}

	k2, err := loadOrCreateKey(path)
	if err != nil || k2 != k1 {
		t.Fatalf("the key must be stable across restarts: %q vs %q (%v)", k1, k2, err)
	}
}

func TestLoadKeyRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name, body string
		mode       os.FileMode
		want       string
	}{
		{"open", "0123456789abcdef0123", 0o644, "must not be accessible"},
		{"short", "abc", 0o600, "at least 16"},
		{"weird", "0123456789abcdef&x=1&", 0o600, "at least 16"},
	}
	for _, tc := range cases {
		if err := os.Chmod(write(tc.name, tc.body, tc.mode), tc.mode); err != nil {
			t.Fatal(err)
		}
		_, err := loadOrCreateKey(filepath.Join(dir, tc.name))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	// A missing parent directory is an error, not silently created.
	if _, err := loadOrCreateKey(filepath.Join(dir, "no", "such", "key")); err == nil {
		t.Error("expected an error for a missing directory")
	}
}

func TestParseKeyFileFlag(t *testing.T) {
	a, err := parseArgs([]string{"start", "--key-file", "/x/key"})
	if err != nil || a.keyFile != "/x/key" {
		t.Fatalf("got %+v, %v", a, err)
	}
	if _, err := parseArgs([]string{"start", "--key-file"}); err == nil {
		t.Error("a missing value must be an error")
	}
}
