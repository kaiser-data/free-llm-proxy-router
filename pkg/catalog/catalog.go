// Package catalog manages the local free-model catalog.
package catalog

import "time"

// CatalogEntry is a single model entry discovered at runtime.
type CatalogEntry struct {
	ProviderID    string    `json:"provider_id"`
	ModelID       string    `json:"model_id"`
	IsFree        bool      `json:"is_free"`
	TierType      string    `json:"tier_type"` // "free" | "credit"
	ContextWindow int       `json:"context_window,omitempty"`
	DiscoveredAt  time.Time `json:"discovered_at"`
	// NeedsReverification is set when a 429 is received on a model marked free.
	NeedsReverification bool `json:"needs_reverification,omitempty"`
	// LastVerifiedAt is the last time this entry was confirmed free.
	LastVerifiedAt time.Time `json:"last_verified_at,omitempty"`
	// Metadata holds provider-specific fields (e.g. pricing, max_tokens).
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Catalog holds all discovered free-model entries.
type Catalog struct {
	Version   int            `json:"version"`
	UpdatedAt time.Time      `json:"updated_at"`
	Entries   []CatalogEntry `json:"entries"`
	// Blocklist contains "provider_id/model_id" pairs that are permanently excluded.
	// Models on this list are removed after every scan (non-existent, chat-incompatible, etc).
	Blocklist []string `json:"blocklist,omitempty"`
}

// FilterBlocklisted removes any entry whose "provider_id/model_id" appears in Blocklist.
func (c *Catalog) FilterBlocklisted() {
	if len(c.Blocklist) == 0 {
		return
	}
	blocked := make(map[string]struct{}, len(c.Blocklist))
	for _, b := range c.Blocklist {
		blocked[b] = struct{}{}
	}
	var kept []CatalogEntry
	for _, e := range c.Entries {
		if _, bad := blocked[e.ProviderID+"/"+e.ModelID]; !bad {
			kept = append(kept, e)
		}
	}
	c.Entries = kept
}

// Block adds a "provider_id/model_id" key to the blocklist if not already present.
func (c *Catalog) Block(providerID, modelID string) {
	key := providerID + "/" + modelID
	for _, b := range c.Blocklist {
		if b == key {
			return
		}
	}
	c.Blocklist = append(c.Blocklist, key)
}

// ByProvider returns all entries for the given provider.
func (c *Catalog) ByProvider(providerID string) []CatalogEntry {
	var out []CatalogEntry
	for _, e := range c.Entries {
		if e.ProviderID == providerID {
			out = append(out, e)
		}
	}
	return out
}

// Find returns the entry for a given provider+model pair, or nil.
func (c *Catalog) Find(providerID, modelID string) *CatalogEntry {
	for i := range c.Entries {
		e := &c.Entries[i]
		if e.ProviderID == providerID && e.ModelID == modelID {
			return e
		}
	}
	return nil
}

// MarkNeedsReverification flags a model for reverification (e.g. after a 429).
func (c *Catalog) MarkNeedsReverification(providerID, modelID string) {
	for i := range c.Entries {
		if c.Entries[i].ProviderID == providerID && c.Entries[i].ModelID == modelID {
			c.Entries[i].NeedsReverification = true
			return
		}
	}
}

// FreeEntries returns only entries marked as free.
func (c *Catalog) FreeEntries() []CatalogEntry {
	var out []CatalogEntry
	for _, e := range c.Entries {
		if e.IsFree {
			out = append(out, e)
		}
	}
	return out
}
