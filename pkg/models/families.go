package models

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

var loadedFamilies []ModelFamily

// LoadFamilies reads family definitions from a YAML file.
// Call this once at startup before using DetectFamily.
func LoadFamilies(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var families []ModelFamily
	if err := yaml.Unmarshal(data, &families); err != nil {
		return err
	}
	loadedFamilies = families
	return nil
}

// DetectFamily returns the family ID for a model ID, or "" if unknown.
// It reads from the YAML-loaded family table; no hardcoding here.
func DetectFamily(modelID string) string {
	lower := strings.ToLower(modelID)
	for _, fam := range loadedFamilies {
		for _, name := range fam.Names {
			if strings.Contains(lower, strings.ToLower(name)) {
				return fam.ID
			}
		}
	}
	return ""
}
