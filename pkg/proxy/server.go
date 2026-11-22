package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kaiser-data/free-llm-proxy-router/pkg/catalog"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/config"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/ratelimit"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/reliability"
	"github.com/kaiser-data/free-llm-proxy-router/pkg/strategy"
)

// Server is the OpenAI-compatible proxy server.
type Server struct {
	conf               atomic.Pointer[configState]
	catalog            atomic.Pointer[catalogSnapshot]
	strategyReg        *strategy.Registry
	rateLimiter        *ratelimit.GlobalTracker
	geminiTracker      *ratelimit.GeminiTracker
	reliabilityTracker *reliability.Tracker
	cache              *ResponseCache
	httpServer         *http.Server
}

// configState pairs a config with the filters built from it. They are stored
// and loaded together, so a request never sees one reload's provider
// settings with another reload's filters.
type configState struct {
	cfg     *config.Config
	filters strategy.Filters
}

// config returns the running configuration.
func (s *Server) config() *config.Config {
	if st := s.conf.Load(); st != nil {
		return st.cfg
	}
	return nil
}

// catalogSnapshot pairs a catalog with its free chat entries. FreeEntries
// filters every entry through IsChatModel and copies the result; doing that
// once per catalog load instead of once per request matters with thousands
// of entries. Both fields are read-only once stored.
type catalogSnapshot struct {
	cat  *catalog.Catalog
	free []catalog.CatalogEntry
}

func newCatalogSnapshot(cat *catalog.Catalog) *catalogSnapshot {
	if cat == nil {
		cat = &catalog.Catalog{}
	}
	free := cat.FreeEntries() // a fresh slice: safe to fill before sharing
	catalog.PrecomputeParams(free)
	if free == nil {
		free = []catalog.CatalogEntry{} // non-nil: FallbackChain.Free nil means "compute"
	}
	return &catalogSnapshot{cat: cat, free: free}
}

// upstreamClient is shared by every upstream call so connections are reused.
// http.DefaultTransport keeps only 2 idle connections per host, so concurrent
// requests to one provider kept re-doing the TLS handshake — slow on the small
// ARM boards this proxy targets.
var upstreamClient = newUpstreamClient()

func newUpstreamClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 16
	// No Client.Timeout: it would also bound reading the body and cut long
	// streams. Each call is bounded by the provider timeout (until the first
	// content chunk) and the request deadline instead.
	return &http.Client{Transport: t}
}

// NewServer creates a new proxy Server.
func NewServer(
	cfg *config.Config,
	cat *catalog.Catalog,
	stratReg *strategy.Registry,
	rateLimiter *ratelimit.GlobalTracker,
	geminiTracker *ratelimit.GeminiTracker,
	reliabilityTracker *reliability.Tracker,
) (*Server, error) {
	s := &Server{
		strategyReg:        stratReg,
		rateLimiter:        rateLimiter,
		geminiTracker:      geminiTracker,
		reliabilityTracker: reliabilityTracker,
	}
	if err := s.UpdateConfig(cfg); err != nil {
		return nil, err
	}
	s.UpdateCatalog(cat)
	s.cache = NewResponseCache(cfg.Proxy.CacheTTL)
	return s, nil
}

// UpdateConfig hot-reloads the configuration. A config whose filters do not
// build is rejected and the current one stays in force.
func (s *Server) UpdateConfig(cfg *config.Config) error {
	filters, err := strategy.BuildFilters(cfg.Proxy.Filters)
	if err != nil {
		return err
	}
	s.conf.Store(&configState{cfg: cfg, filters: filters})

	// Copy provider endpoints before starting asynchronous lookups. The
	// running config may be mutated by a reload or test, and must not be
	// shared with the DNS goroutine.
	go preResolveProviderHosts(providerHosts(cfg))
	return nil
}

// providerHosts returns the immutable set of hostnames needed for DNS
// pre-resolution.
func providerHosts(cfg *config.Config) []string {
	seen := make(map[string]bool)
	hosts := make([]string, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if !p.Enabled || p.BaseURL == "" {
			continue
		}
		host := extractHost(p.BaseURL)
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	return hosts
}

// preResolveProviderHosts looks up already-copied hostnames. It never retains
// or reads the configuration used to derive them.
func preResolveProviderHosts(hosts []string) {
	for _, host := range hosts {
		host := host
		go func() {
			if _, err := net.LookupHost(host); err != nil {
				log.Printf("dns pre-resolve: %s: %v", host, err)
			} else {
				log.Printf("dns pre-resolve: %s ok", host)
			}
		}()
	}
}

// extractHost returns only the hostname from an absolute provider URL.
func extractHost(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	if u.Hostname() != "" {
		return u.Hostname()
	}
	return ""
}

// UpdateCatalog hot-reloads the model catalog.
func (s *Server) UpdateCatalog(cat *catalog.Catalog) {
	s.catalog.Store(newCatalogSnapshot(cat))
}

