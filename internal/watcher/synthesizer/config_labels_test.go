package synthesizer

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAPIKeyLabelsPreserveCredentialIdentity(t *testing.T) {
	for _, family := range []string{"gemini", "interactions", "claude", "codex", "xai", "meta", "vertex", "openai-compatibility"} {
		t.Run(family, func(t *testing.T) {
			load := func(label string) (*config.Config, []*coreauth.Auth) {
				t.Helper()
				data := []byte(fmt.Sprintf("api-keys:\n  %s:\n    - name: work\n      base-url: https://api.example.test/v1\n      keys:\n        - api-key: fake-key-one\n          label: %q\n        - api-key: fake-key-two\n          label: Second account\n", family, label))
				if err := config.ValidateV8Config(data); err != nil {
					t.Fatal(err)
				}
				cfg, err := config.ParseConfigBytes(data)
				if err != nil {
					t.Fatal(err)
				}
				ctx := &SynthesisContext{Config: cfg, Now: time.Unix(1, 0), IDGenerator: NewStableIDGenerator()}
				auths, err := NewConfigSynthesizer().Synthesize(ctx)
				if err != nil || len(auths) != 2 {
					t.Fatalf("synthesize: count=%d error=%v", len(auths), err)
				}
				return cfg, auths
			}
			_, before := load("")
			cfg, named := load("  Personal account  ")
			if named[0].Label != "Personal account" || named[1].Label != "Second account" {
				t.Fatalf("per-key names not applied: %q, %q", named[0].Label, named[1].Label)
			}
			for i := range before {
				copyBefore, copyNamed := *before[i], *named[i]
				copyBefore.Label, copyNamed.Label = "", ""
				if !reflect.DeepEqual(copyBefore, copyNamed) {
					t.Fatal("rename changed credential identity, routing, or selection metadata")
				}
			}
			_, cleared := load(" \t ")
			if cleared[0].Label != before[0].Label {
				t.Fatalf("blank name did not restore default %q", before[0].Label)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("api-keys: {}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := config.SaveConfigPreserveComments(path, cfg); err != nil {
				t.Fatal(err)
			}
			reloaded, err := config.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			after, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{Config: reloaded, Now: time.Unix(1, 0), IDGenerator: NewStableIDGenerator()})
			if err != nil || len(after) != 2 || after[0].Label != named[0].Label || after[1].Label != named[1].Label || after[0].ID != named[0].ID {
				t.Fatalf("save/reload lost key labels or identity: %v", err)
			}
		})
	}
}
