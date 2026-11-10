package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenFileBounds(t *testing.T) {
	for _, tt := range []struct {
		value string
		valid bool
	}{{strings.Repeat("x", 32) + "\n", true}, {"short", false}, {strings.Repeat("x", 4096), false}, {strings.Repeat("x", 32) + "\nextra", false}} {
		p := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(p, []byte(tt.value), 0600); err != nil {
			t.Fatal(err)
		}
		s, err := readToken(p)
		if (err == nil) != tt.valid {
			t.Fatal(tt.valid, err)
		}
		if tt.valid && s != strings.Repeat("x", 32) {
			t.Fatal("newline not stripped")
		}
	}
}
