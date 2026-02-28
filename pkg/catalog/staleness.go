package catalog

import (
	"os"
	"time"
)

// IsStale returns true if the catalog file is older than maxAge.
func IsStale(path string, maxAge time.Duration) bool {
	info, err := os.Stat(expandHome(path))
	if err != nil {
		return true // missing = stale
	}
	return time.Since(info.ModTime()) > maxAge
}

// NeedsEnrichment returns true if the enriched catalog is missing or older than
// the catalog file (i.e. the catalog was updated more recently).
func NeedsEnrichment(catalogPath, enrichedPath string) bool {
	catInfo, err := os.Stat(expandHome(catalogPath))
	if err != nil {
		return false // no catalog yet — nothing to enrich
	}
	enrichedInfo, err := os.Stat(expandHome(enrichedPath))
	if err != nil {
		return true // enriched missing
	}
	return catInfo.ModTime().After(enrichedInfo.ModTime())
}

// PendingReverification returns the list of model IDs (per provider) that need
// re-verification after unexpected 429s.
func PendingReverification(c *Catalog) []CatalogEntry {
	var out []CatalogEntry
	for _, e := range c.Entries {
		if e.NeedsReverification {
			out = append(out, e)
		}
	}
	return out
}
