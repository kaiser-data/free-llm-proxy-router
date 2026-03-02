// picoclaw-scan: Free model discovery and catalog management.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/kaiser-data/picoclaw-free-llm/pkg/catalog"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/config"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/scan"
)

var cfgFile string

func main() {
	root := &cobra.Command{
		Use:   "picoclaw-scan",
		Short: "Discover and manage free LLM model catalog",
	}
	root.PersistentFlags().StringVarP(&cfgFile, "config", "c", "", "config file path")

	root.AddCommand(
		updateCmd(),
		probeCmd(),
		refreshLLMCmd(),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// updateCmd scans all enabled providers and writes catalog.json.
func updateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Scan all enabled providers and update catalog.json",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			// Load existing catalog to preserve blocklist across scans.
			existing, _ := catalog.Load(cfg.Catalog.Path)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			dispatcher := scan.NewDispatcher()
			cat, err := dispatcher.ScanAll(ctx, cfg.Providers)
			if err != nil {
				return fmt.Errorf("scanning providers: %w", err)
			}

			// Carry forward the blocklist from the previous catalog.
			if existing != nil {
				cat.Blocklist = existing.Blocklist
			}
			cat.FilterBlocklisted()

			// Correlate cross-provider families
			correlated := scan.Correlate(cat.Entries)
			log.Printf("scan: found %d model families across %d entries", len(correlated), len(cat.Entries))

			if err := catalog.Save(cat, cfg.Catalog.Path); err != nil {
				return fmt.Errorf("saving catalog: %w", err)
			}
			log.Printf("scan: wrote %d entries to %s", len(cat.Entries), cfg.Catalog.Path)
			return nil
		},
	}
}

// probeCmd probes models that need reverification.
func probeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "probe",
		Short: "Probe models flagged as needing reverification",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			cat, err := catalog.Load(cfg.Catalog.Path)
			if err != nil {
				return fmt.Errorf("loading catalog: %w", err)
			}

			pending := catalog.PendingReverification(cat)
			if len(pending) == 0 {
				log.Printf("probe: no models need reverification")
				return nil
			}
			log.Printf("probe: checking %d models...", len(pending))

			prober := &scan.Prober{Client: &http.Client{Timeout: 20 * time.Second}}
			ctx := context.Background()
			updated := 0
			for _, e := range pending {
				// Find provider config
				var provCfg *config.ProviderConfig
				for i := range cfg.Providers {
					if cfg.Providers[i].ID == e.ProviderID {
						provCfg = &cfg.Providers[i]
						break
					}
				}
				if provCfg == nil {
					continue
				}
				result := prober.Probe(ctx, e, *provCfg)
				entry := cat.Find(e.ProviderID, e.ModelID)
				if entry == nil {
					continue
				}
				if result.IsAccessible {
					entry.NeedsReverification = false
					entry.LastVerifiedAt = time.Now()
					log.Printf("probe: %s/%s ✓ still free", e.ProviderID, e.ModelID)
				} else if result.StatusCode == 402 || result.StatusCode == 403 {
					entry.IsFree = false
					log.Printf("probe: %s/%s ✗ no longer free (status %d)", e.ProviderID, e.ModelID, result.StatusCode)
				} else {
					log.Printf("probe: %s/%s ? status %d (keeping as free)", e.ProviderID, e.ModelID, result.StatusCode)
				}
				updated++
			}

			if err := catalog.Save(cat, cfg.Catalog.Path); err != nil {
				return fmt.Errorf("saving catalog: %w", err)
			}
			log.Printf("probe: updated %d entries", updated)
			return nil
		},
	}
}

// refreshLLMCmd uses an LLM to diff the catalog against live provider info.
func refreshLLMCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh-llm",
		Short: "Use LLM to check for free tier changes and update catalog diff",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			cat, err := catalog.Load(cfg.Catalog.Path)
			if err != nil {
				return fmt.Errorf("loading catalog: %w", err)
			}

			promptFile := cfg.Refresh.PromptFile
			if promptFile == "" {
				promptFile = "configs/refresh-prompt.txt"
			}

			refresher := &scan.LLMRefresher{
				Client:     &http.Client{Timeout: 60 * time.Second},
				PromptFile: promptFile,
				Cfg:        &cfg.Refresh,
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			diff, err := refresher.Refresh(ctx, cat, cfg.Providers)
			if err != nil {
				return fmt.Errorf("LLM refresh: %w", err)
			}

			log.Printf("refresh: changed=%d, added=%d, removed_confidence=%d",
				len(diff.Changed), len(diff.Added), len(diff.RemovedConfidence))

			scan.MergeDiff(cat, diff)

			if err := catalog.Save(cat, cfg.Catalog.Path); err != nil {
				return fmt.Errorf("saving catalog: %w", err)
			}
			log.Printf("refresh: catalog updated at %s", cfg.Catalog.Path)
			return nil
		},
	}
}
