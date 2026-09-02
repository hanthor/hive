package config

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRotationConfig_YAMLRoundTrip(t *testing.T) {
	in := RotationConfig{
		Enabled:            true,
		ThresholdPct:       90,
		HighVolumeCadenceS: 900,
		Providers: map[string]ProviderRotationConfig{
			"anthropic": {Class: "subscription", Backends: []string{"claude", "pi"}},
			"deepseek":  {Class: "metered", Backends: []string{"litellm"}},
		},
		AgentTiers: map[string]string{"worker": "T1", "triage": "T3"},
	}
	data, err := yaml.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out RotationConfig
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round-trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

func TestRotationConfig_Defaults(t *testing.T) {
	var r RotationConfig
	if r.Enabled {
		t.Error("Enabled default = true, want false (opt-in)")
	}
	if got := r.EffectiveThreshold(); got != 85 {
		t.Errorf("EffectiveThreshold = %d, want 85", got)
	}
	if got := r.EffectiveHighVolumeCadenceS(); got != 1800 {
		t.Errorf("EffectiveHighVolumeCadenceS = %d, want 1800", got)
	}
	r.ThresholdPct = 70
	r.HighVolumeCadenceS = 600
	if got := r.EffectiveThreshold(); got != 70 {
		t.Errorf("EffectiveThreshold = %d, want 70", got)
	}
	if got := r.EffectiveHighVolumeCadenceS(); got != 600 {
		t.Errorf("EffectiveHighVolumeCadenceS = %d, want 600", got)
	}
}

func TestRotationConfig_YAMLParse(t *testing.T) {
	src := `
enabled: true
threshold_pct: 85
high_volume_cadence_s: 1800
providers:
  anthropic:
    class: subscription
    backends: [claude, pi]
  deepseek:
    class: metered
    backends: [litellm]
agents:
  worker: T1
`
	var r RotationConfig
	if err := yaml.Unmarshal([]byte(src), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !r.Enabled || r.ThresholdPct != 85 || r.HighVolumeCadenceS != 1800 {
		t.Errorf("parsed = %+v", r)
	}
	if got := r.Providers["anthropic"].Class; got != "subscription" {
		t.Errorf("anthropic class = %q", got)
	}
	if got := r.AgentTiers["worker"]; got != "T1" {
		t.Errorf("worker tier = %q", got)
	}
}

func TestRotationConfig_DefaultProviders(t *testing.T) {
	defaults := DefaultRotationProviders()
	expected := map[string][]string{
		"anthropic": {"claude", "pi"},
		"openai":    {"codex"},
		"google":    {"agy"},
		"github":    {"copilot"},
		"deepseek":  {"litellm"},
	}
	for prov, backends := range expected {
		pc, ok := defaults[prov]
		if !ok {
			t.Errorf("missing default provider %q", prov)
			continue
		}
		for _, b := range backends {
			found := false
			for _, pb := range pc.Backends {
				if pb == b {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("provider %q missing backend %q in defaults", prov, b)
			}
		}
	}
}

func TestRotationConfig_EffectiveProvidersAndHasBackend(t *testing.T) {
	var empty RotationConfig
	eff := empty.EffectiveProviders()
	if _, ok := eff["github"]; !ok {
		t.Error("EffectiveProviders() missing github provider by default")
	}
	if !empty.HasBackend("copilot") {
		t.Error("HasBackend(copilot) = false, want true for defaults")
	}
	if !empty.HasBackend("claude") {
		t.Error("HasBackend(claude) = false, want true for defaults")
	}
	if empty.HasBackend("nonexistent-backend") {
		t.Error("HasBackend(nonexistent-backend) = true, want false")
	}

	custom := RotationConfig{
		Providers: map[string]ProviderRotationConfig{
			"custom": {Class: "subscription", Backends: []string{"my-backend"}},
		},
	}
	if !custom.HasBackend("my-backend") {
		t.Error("custom.HasBackend(my-backend) = false, want true")
	}
	if custom.HasBackend("copilot") {
		t.Error("custom.HasBackend(copilot) = true, want false when overridden")
	}
}

func TestConfig_ValidatePackAgentsHaveRotationRung(t *testing.T) {
	// Standard config with default rotation should pass validation
	cfg := &Config{
		Project: ProjectConfig{Org: "testorg", Repos: []string{"r"}},
		GitHub:  GitHubConfig{Token: "test-token"},
		Agents: map[string]AgentConfig{
			"guide": {Backend: "copilot", Enabled: true},
		},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("cfg.validate() error = %v, want nil", err)
	}

	// If rotation providers are overridden to something that excludes copilot,
	// validation must catch that pack agents default to an unrotatable backend.
	cfgBad := &Config{
		Project: ProjectConfig{Org: "testorg", Repos: []string{"r"}},
		GitHub:  GitHubConfig{Token: "test-token"},
		Agents: map[string]AgentConfig{
			"quality": {Backend: "claude", Enabled: true},
		},
		Governor: GovernorConfig{
			Rotation: RotationConfig{
				Providers: map[string]ProviderRotationConfig{
					"anthropic": {Class: "subscription", Backends: []string{"claude"}},
				},
			},
		},
	}
	if err := cfgBad.validate(); err == nil {
		t.Error("cfgBad.validate() = nil, want error when pack agents have no rotation tier rung")
	}
}

