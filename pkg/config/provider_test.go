package config

import "testing"

func TestMissingKey(t *testing.T) {
	t.Setenv("FLPR_TEST_KEY", "")
	needsKey := ProviderConfig{ID: "groq", APIKeyEnv: "FLPR_TEST_KEY"}
	if !needsKey.MissingKey() {
		t.Error("provider with api_key_env and no key should report MissingKey")
	}

	t.Setenv("FLPR_TEST_KEY", "gsk_test")
	if needsKey.MissingKey() {
		t.Error("provider with key in env should not report MissingKey")
	}

	literal := ProviderConfig{ID: "x", APIKeyEnv: "FLPR_UNSET_KEY", APIKey: "literal"}
	if literal.MissingKey() {
		t.Error("literal api_key should count as configured")
	}

	keyless := ProviderConfig{ID: "ollama-local"}
	if keyless.MissingKey() {
		t.Error("keyless provider (no api_key_env) must never report MissingKey")
	}
}
