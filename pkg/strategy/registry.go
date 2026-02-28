package strategy

import (
	"fmt"

	"github.com/kaiser-data/picoclaw-free-llm/pkg/reliability"
	"github.com/kaiser-data/picoclaw-free-llm/pkg/ratelimit"
)

// Registry maps strategy names to Strategy instances.
type Registry struct {
	strategies map[string]Strategy
}

// NewRegistry creates a Registry with all 13 strategies pre-registered.
// Pass the dependency objects needed by stateful strategies.
func NewRegistry(
	reliabilityTracker *reliability.Tracker,
	globalRateTracker *ratelimit.GlobalTracker,
	similarFamily string,
	geminiPreferredModel string,
	parallelFanOut int,
	orFallbackN int,
) *Registry {
	r := &Registry{strategies: make(map[string]Strategy)}

	strategies := []Strategy{
		&StrategyPerformance{},
		&StrategySpeed{},
		&StrategyVolume{GeminiPreferredModel: geminiPreferredModel},
		&StrategyBalanced{},
		&StrategySmall{GlobalTracker: globalRateTracker},
		&StrategyTiny{},
		&StrategyCoding{},
		&StrategyLongContext{},
		&StrategySimilar{TargetFamily: similarFamily},
		&StrategyParallel{FanOut: parallelFanOut},
		&StrategyReliable{Tracker: reliabilityTracker},
		&StrategyEconomical{},
		&StrategyAdaptive{
			Tracker:                   reliabilityTracker,
			OpenRouterNativeFallbackN: orFallbackN,
		},
	}

	for _, s := range strategies {
		r.strategies[s.Name()] = s
	}
	return r
}

// Get retrieves a strategy by name.
func (r *Registry) Get(name string) (Strategy, error) {
	s, ok := r.strategies[name]
	if !ok {
		return nil, fmt.Errorf("unknown strategy: %q (available: %v)", name, r.Names())
	}
	return s, nil
}

// Names returns all registered strategy names.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.strategies))
	for k := range r.strategies {
		names = append(names, k)
	}
	return names
}
