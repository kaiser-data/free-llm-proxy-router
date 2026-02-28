package bench

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Report aggregates bench results per strategy.
type Report struct {
	ByStrategy map[string][]BenchMetrics `json:"by_strategy"`
}

// GenerateReport builds a Report from a flat slice of BenchMetrics.
func GenerateReport(metrics []BenchMetrics) *Report {
	r := &Report{ByStrategy: make(map[string][]BenchMetrics)}
	for _, m := range metrics {
		r.ByStrategy[m.StrategyName] = append(r.ByStrategy[m.StrategyName], m)
	}
	// Sort each strategy's results: successful first, then by tokens/sec desc
	for k, ms := range r.ByStrategy {
		sort.Slice(ms, func(i, j int) bool {
			if ms[i].Success != ms[j].Success {
				return ms[i].Success // success before failure
			}
			return ms[i].TokensPerSecond() > ms[j].TokensPerSecond()
		})
		r.ByStrategy[k] = ms
	}
	return r
}

// WriteJSON writes the report as JSON to w.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteHuman writes a human-readable report to w.
func (r *Report) WriteHuman(w io.Writer) {
	// Sort strategies alphabetically
	strategies := make([]string, 0, len(r.ByStrategy))
	for k := range r.ByStrategy {
		strategies = append(strategies, k)
	}
	sort.Strings(strategies)

	fmt.Fprintln(w, strings.Repeat("=", 70))
	fmt.Fprintln(w, "PicoClaw Free LLM Benchmark Report")
	fmt.Fprintln(w, strings.Repeat("=", 70))

	for _, strat := range strategies {
		metrics := r.ByStrategy[strat]
		successCount := 0
		for _, m := range metrics {
			if m.Success {
				successCount++
			}
		}
		fmt.Fprintf(w, "\nStrategy: %-20s (%d/%d successful)\n",
			strat, successCount, len(metrics))
		fmt.Fprintln(w, strings.Repeat("-", 60))

		for i, m := range metrics {
			status := "✓"
			if !m.Success {
				status = "✗"
			}
			tps := m.TokensPerSecond()
			fmt.Fprintf(w, "  %d. %s %s/%s\n", i+1, status, m.ProviderID, m.ModelID)
			fmt.Fprintf(w, "     TTFT: %v | Total: %v | %.0f tok/s",
				m.TTFT.Truncate(1), m.TotalDuration.Truncate(1), tps)
			if m.ReasoningTokens > 0 {
				fmt.Fprintf(w, " | reasoning: %d tokens", m.ReasoningTokens)
			}
			if !m.Success {
				fmt.Fprintf(w, " | error: %s", m.ErrorMsg)
			}
			fmt.Fprintln(w)
		}
	}
	fmt.Fprintln(w, strings.Repeat("=", 70))
}
