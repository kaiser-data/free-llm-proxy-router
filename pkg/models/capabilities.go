package models

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

var loadedCapabilityPatterns []KnownCapabilityPattern

// LoadCapabilities reads capability patterns from a YAML file.
// Call this once at startup before using DetectCapabilities.
func LoadCapabilities(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var patterns []KnownCapabilityPattern
	if err := yaml.Unmarshal(data, &patterns); err != nil {
		return err
	}
	loadedCapabilityPatterns = patterns
	return nil
}

// DetectCapabilities returns all capabilities inferred from a model ID string.
// Matching is based on the YAML patterns; no capabilities are hardcoded here.
func DetectCapabilities(modelID string) []Capability {
	lower := strings.ToLower(modelID)
	seen := map[Capability]bool{}
	var caps []Capability

	for _, pattern := range loadedCapabilityPatterns {
		for _, kw := range pattern.Keywords {
			if strings.Contains(lower, strings.ToLower(kw)) {
				if !seen[pattern.Capability] {
					seen[pattern.Capability] = true
					caps = append(caps, pattern.Capability)
				}
				break
			}
		}
	}
	return caps
}
