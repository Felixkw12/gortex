package llm

import "testing"

// withPreset installs a gateway preset for the duration of a test.
func withPreset(t *testing.T, name string, cp CustomProvider) {
	t.Helper()
	gatewayPresets[name] = cp
	t.Cleanup(func() { delete(gatewayPresets, name) })
}

func TestPreset_ReturnsCopy(t *testing.T) {
	withPreset(t, "testgw", CustomProvider{BaseURL: "https://preset/v1", Model: "m", Headers: map[string]string{"x": "1"}})
	cp, ok := Preset(" TestGW ")
	if !ok {
		t.Fatal("preset lookup should be case-insensitive")
	}
	cp.Headers["x"] = "mutated"
	if again, _ := Preset("testgw"); again.Headers["x"] != "1" {
		t.Error("Preset must hand out a copy of the headers map")
	}
	if _, ok := Preset("nope"); ok {
		t.Error("unknown preset must not resolve")
	}
}

func TestPresetNames_SortedAndNotReserved(t *testing.T) {
	names := PresetNames()
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("PresetNames not sorted: %v", names)
		}
	}
	for _, name := range names {
		if _, ok := Preset(name); !ok {
			t.Errorf("listed preset %q does not resolve", name)
		}
	}
}

// TestConfig_ActiveCustom_PresetResolution pins the resolution order for
// llm.provider: a true built-in, then the user's custom provider, then
// a gateway preset.
func TestConfig_ActiveCustom_PresetResolution(t *testing.T) {
	preset := CustomProvider{BaseURL: "https://preset/v1", Model: "preset-model", APIKeyEnv: "PRESET_KEY", PromptProfile: "frontier"}
	withPreset(t, "testgw", preset)
	withPreset(t, "openai", preset) // a built-in name must never resolve to a preset

	// Preset only.
	c := Config{Provider: "testgw"}
	cp, ok := c.ActiveCustom()
	if !ok || cp.BaseURL != "https://preset/v1" || cp.Model != "preset-model" {
		t.Fatalf("preset not resolved: %+v ok=%v", cp, ok)
	}
	if !c.IsEnabled() || c.ActiveModel() != "preset-model" || ProfileFor(c) != ProfileFrontier {
		t.Errorf("preset config: enabled=%v model=%q profile=%v", c.IsEnabled(), c.ActiveModel(), ProfileFor(c))
	}

	// A user custom provider of the same name always wins, wholesale —
	// no preset field leaks into it.
	c.Custom = map[string]CustomProvider{"testgw": {BaseURL: "https://mine/v1", Model: "my-model"}}
	cp, _ = c.ActiveCustom()
	if cp.BaseURL != "https://mine/v1" || cp.Model != "my-model" || cp.APIKeyEnv != "" || cp.PromptProfile != "" {
		t.Errorf("user entry must win over the preset: %+v", cp)
	}
	if ProfileFor(c) != ProfileSmall {
		t.Error("user entry without prompt_profile must not inherit the preset's tier")
	}

	// Env / routing overrides apply on top of a preset.
	t.Setenv("GORTEX_LLM_PROVIDER", "testgw")
	t.Setenv("GORTEX_LLM_MODEL", "env-model")
	t.Setenv("GORTEX_LLM_EFFORT", "high")
	c = Config{}.MergeEnv()
	cp, _ = c.ActiveCustom()
	if cp.Model != "env-model" || cp.ReasoningEffort != "high" || cp.BaseURL != "https://preset/v1" {
		t.Errorf("env overrides over preset: %+v", cp)
	}
	if got := c.WithModel("routed").ActiveModel(); got != "routed" {
		t.Errorf("WithModel over preset = %q want routed", got)
	}

	// A built-in name never resolves to a preset.
	if _, ok := (Config{Provider: "openai"}).ActiveCustom(); ok {
		t.Error("built-in provider must not resolve to a preset")
	}
}
