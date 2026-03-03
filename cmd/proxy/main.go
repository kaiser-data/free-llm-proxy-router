// free-llm-proxy: OpenAI-compatible proxy with intelligent routing and fallback.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/models"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/proxy"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/scan"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

var (
	cfgFile  string
	port     int
	stratName string
)

func main() {
	root := &cobra.Command{
		Use:   "free-llm-proxy",
		Short: "OpenAI-compatible proxy for free LLM providers",
		RunE:  runProxy,
	}
	root.Flags().StringVarP(&cfgFile, "config", "c", "", "config file path (default: search in . and ~/.free-llm-proxy-router/)")
	root.Flags().IntVarP(&port, "port", "p", 0, "override proxy port (default from config: 8080)")
	root.Flags().StringVarP(&stratName, "strategy", "s", "", "override strategy name")

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runProxy(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if port > 0 {
		cfg.Proxy.Port = port
	}
	if stratName != "" {
		cfg.Proxy.Strategy = stratName
	}

	// Load model catalog
	cat, err := catalog.Load(cfg.Catalog.Path)
	if err != nil {
		return fmt.Errorf("loading catalog: %w", err)
	}
	freeCount := len(cat.FreeEntries())
	log.Printf("catalog: %d free model(s) loaded from %s", freeCount, cfg.Catalog.Path)
	if freeCount == 0 {
		log.Printf("warning: catalog is empty — run 'free-llm-scan update' to discover free models")
	}

	// Check catalog staleness
	if catalog.IsStale(cfg.Catalog.Path, time.Duration(cfg.Catalog.MaxAgeHours)*time.Hour) {
		log.Printf("warning: catalog is older than %dh — consider running 'free-llm-scan update'",
			cfg.Catalog.MaxAgeHours)
	}

	// Load capability/family YAML data if present
	if err := models.LoadFamilies("configs/families.yaml"); err != nil {
		log.Printf("families.yaml not found or invalid: %v (family detection disabled)", err)
	}
	if err := models.LoadCapabilities("configs/capabilities.yaml"); err != nil {
		log.Printf("capabilities.yaml not found or invalid: %v (capability detection disabled)", err)
	}

	// Set up rate limiters
	rateLimiter := ratelimit.NewGlobalTracker()
	geminiTracker := ratelimit.NewGeminiTracker()

	// Load persisted rate-limit usage
	usagePath := "~/.free-llm-proxy-router/usage.json"
	if _, err := ratelimit.LoadUsage(usagePath); err != nil {
		log.Printf("usage.json not found (starting fresh): %v", err)
	}

	// Set up reliability tracker
	reliabilityTracker := reliability.New()
	relPath := "~/.free-llm-proxy-router/reliability.json"
	if err := reliability.Load(reliabilityTracker, relPath); err != nil {
		log.Printf("reliability.json not found (starting fresh): %v", err)
	}

	// Set up strategy registry
	similarFamily := ""
	if cfg.Proxy.Similar != nil {
		similarFamily = cfg.Proxy.Similar["model_family"]
	}
	geminiPreferred := ""
	for _, p := range cfg.Providers {
		if p.ID == "gemini" {
			geminiPreferred = p.PreferredVolumeModel
		}
	}
	fanOut := 3
	if cfg.Proxy.StrategyOverrides != nil {
		if pOv, ok := cfg.Proxy.StrategyOverrides["parallel"]; ok {
			if fo, ok := pOv["fan_out"].(int); ok {
				fanOut = fo
			}
		}
	}

	stratReg := strategy.NewRegistry(reliabilityTracker, rateLimiter, similarFamily, geminiPreferred, fanOut, 5)

	// Create server
	srv := proxy.NewServer(cfg, cat, stratReg, rateLimiter, geminiTracker, reliabilityTracker)

	// Handle graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Watch catalog file for hot-reload (triggered by git-sync pull or manual update).
	stopCatalogWatch, err := catalog.Watch(cfg.Catalog.Path, func(newCat *catalog.Catalog) {
		srv.UpdateCatalog(newCat)
		log.Printf("catalog: hot-reloaded (%d models)", len(newCat.FreeEntries()))
	})
	if err != nil {
		log.Printf("catalog watch error (hot-reload disabled): %v", err)
	} else {
		defer stopCatalogWatch()
	}

	// Start catalog git-sync (scanner or replica role, depending on config).
	// scanFn is only invoked on the scanner machine; replicas leave it nil.
	scanFn := func(scanCtx context.Context) error {
		existing, _ := catalog.Load(cfg.Catalog.Path)
		dispatcher := scan.NewDispatcher()
		newCat, err := dispatcher.ScanAll(scanCtx, cfg.Providers)
		if err != nil {
			return err
		}
		if existing != nil {
			newCat.Blocklist = existing.Blocklist
		}
		newCat.FilterBlocklisted()
		return catalog.Save(newCat, cfg.Catalog.Path)
	}
	catalog.StartSync(ctx, cfg.Catalog.Path,
		catalog.GitSyncConfig(cfg.Catalog.GitSync), scanFn)

	// Watch config for hot-reload
	stopWatch, err := config.Watch(cfgFile, func(newCfg *config.Config) {
		srv.UpdateConfig(newCfg)
		log.Printf("proxy: config hot-reloaded")
	})
	if err != nil {
		log.Printf("config watch error (hot-reload disabled): %v", err)
	} else {
		defer stopWatch()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("proxy: shutting down...")
		// Save reliability stats on shutdown
		if err := reliability.Save(reliabilityTracker, relPath); err != nil {
			log.Printf("saving reliability: %v", err)
		}
		cancel()
	}()

	return srv.Start(ctx)
}
