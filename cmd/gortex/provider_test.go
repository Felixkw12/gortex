package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/zzet/gortex/internal/llm/registry"
)

// isolateProviders points the providers.json registry at a temp config
// dir so the tests never touch the developer's real ~/.gortex.
func isolateProviders(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(registry.LocalOptInEnv, "")
}

// runProviderAddWith runs `provider add <name>` with the given flags,
// resetting the package-level flag state afterwards.
func runProviderAddWith(t *testing.T, name string, flags map[string]string) (string, error) {
	t.Helper()
	fs := providerAddCmd.Flags()
	t.Cleanup(func() {
		fs.VisitAll(func(f *pflag.Flag) {
			if f.Value.Type() != "stringArray" {
				_ = f.Value.Set(f.DefValue)
			}
			f.Changed = false
		})
		providerHeaders = nil
	})
	for k, v := range flags {
		if err := fs.Set(k, v); err != nil {
			t.Fatalf("set --%s: %v", k, err)
		}
	}
	var out bytes.Buffer
	providerAddCmd.SetOut(&out)
	err := runProviderAdd(providerAddCmd, []string{name})
	return out.String(), err
}

func runProviderShowOut(t *testing.T, name string) string {
	t.Helper()
	var out bytes.Buffer
	providerShowCmd.SetOut(&out)
	if err := runProviderShow(providerShowCmd, []string{name}); err != nil {
		t.Fatalf("provider show %s: %v", name, err)
	}
	return out.String()
}

func runProviderListOut(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	providerListCmd.SetOut(&out)
	if err := runProviderList(providerListCmd, nil); err != nil {
		t.Fatalf("provider list: %v", err)
	}
	return out.String()
}

func TestProviderAdd_PromptProfile(t *testing.T) {
	isolateProviders(t)
	if _, err := runProviderAddWith(t, "gw", map[string]string{
		"base-url": "https://gw.example/v1", "model": "big-model", "prompt-profile": "frontier",
	}); err != nil {
		t.Fatal(err)
	}
	cp, ok, err := registry.Get("gw")
	if err != nil || !ok || cp.PromptProfile != "frontier" {
		t.Fatalf("stored entry %+v ok=%v err=%v, want prompt_profile frontier", cp, ok, err)
	}
	if out := runProviderShowOut(t, "gw"); !strings.Contains(out, "prompt_profile:   frontier") {
		t.Errorf("show output missing prompt_profile:\n%s", out)
	}
	if out := runProviderListOut(t); !strings.Contains(out, "PROFILE") || !strings.Contains(out, "frontier") {
		t.Errorf("list output missing profile column:\n%s", out)
	}
}

func TestProviderAdd_PromptProfileDefaultAndInvalid(t *testing.T) {
	isolateProviders(t)
	if _, err := runProviderAddWith(t, "plain", map[string]string{
		"base-url": "https://gw.example/v1", "model": "m",
	}); err != nil {
		t.Fatal(err)
	}
	if out := runProviderShowOut(t, "plain"); !strings.Contains(out, "prompt_profile:   small (default)") {
		t.Errorf("show output should name the small default:\n%s", out)
	}
	if _, err := runProviderAddWith(t, "bad", map[string]string{
		"base-url": "https://gw.example/v1", "model": "m", "prompt-profile": "huge",
	}); err == nil {
		t.Error("an unknown --prompt-profile must be rejected")
	}
}
