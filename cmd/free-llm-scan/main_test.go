package main

import (
	"strings"
	"testing"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
)

func TestCheckScanTargets(t *testing.T) {
	cases := []struct {
		name      string
		providers []config.ProviderConfig
		wantErr   string
	}{
		{"no config at all", nil, "no providers configured"},
		{"all disabled", []config.ProviderConfig{{ID: "a"}, {ID: "b"}}, "--all-providers"},
		{"one enabled", []config.ProviderConfig{{ID: "a"}, {ID: "b", Enabled: true}}, ""},
	}
	for _, c := range cases {
		err := checkScanTargets(c.providers)
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: err = %v, want it to mention %q", c.name, err, c.wantErr)
		}
	}
}
