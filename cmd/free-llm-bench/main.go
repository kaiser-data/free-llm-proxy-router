// free-llm-bench: Benchmark runner for all strategies and free models.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/bench"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

var (
	cfgFile    string
	outputJSON bool
	outputFile string
)

func main() {
	root := &cobra.Command{
		Use:   "free-llm-bench",
		Short: "Run benchmarks across all strategies and free models",
		RunE:  runBench,
	}
	root.Flags().StringVarP(&cfgFile, "config", "c", "", "config file path")
	root.Flags().BoolVar(&outputJSON, "json", false, "output results as JSON")
	root.Flags().StringVarP(&outputFile, "output", "o", "", "write results to file (default: stdout)")

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runBench(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	cat, err := catalog.Load(cfg.Catalog.Path)
	if err != nil {
		return fmt.Errorf("loading catalog: %w", err)
	}

	// Build all strategies
	relTracker := reliability.New()
	reliability.Load(relTracker, "~/.free-llm-proxy-router/reliability.json")
	rateLimiter := ratelimit.NewGlobalTracker()
	stratReg := strategy.NewRegistry(relTracker, rateLimiter, "", "gemini-2.0-flash-lite", 3, 5)

	var strategies []strategy.Strategy
	for _, name := range stratReg.Names() {
		s, _ := stratReg.Get(name)
		strategies = append(strategies, s)
	}

	runner := &bench.Runner{
		Cfg:        cfg,
		Catalog:    cat,
		Strategies: strategies,
		Client:     &http.Client{Timeout: 60 * time.Second},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	metrics, err := runner.Run(ctx)
	if err != nil {
		return fmt.Errorf("running benchmarks: %w", err)
	}

	report := bench.GenerateReport(metrics)

	// Select output writer
	out := os.Stdout
	if outputFile != "" {
		f, err := os.Create(outputFile)
		if err != nil {
			return fmt.Errorf("creating output file: %w", err)
		}
		defer f.Close()
		out = f
	}

	if outputJSON {
		return report.WriteJSON(out)
	}
	report.WriteHuman(out)
	return nil
}
