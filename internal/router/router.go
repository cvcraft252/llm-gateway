package router

import (
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/cvcraft252/llm-gateway/internal/config"
)

// ErrModelNotFound is returned by Pick when no upstream declares the requested model.
var ErrModelNotFound = errors.New("model not found")

// Upstream is a resolved, ready-to-route upstream provider. It is immutable
// after construction and safe for concurrent reads.
type Upstream struct {
	Name    string
	URL     *url.URL
	Key     string
	Aliases map[string]string
}

// Router maps incoming model names to the upstream that serves them. The map
// is built once at startup and never mutated, so all reads are concurrency-safe
// without synchronization.
type Router struct {
	byModel        map[string]*Upstream
	requestTimeout time.Duration
}

// RequestTimeout returns the per-request timeout configured via routing.timeout.
func (r *Router) RequestTimeout() time.Duration {
	return r.requestTimeout
}

// Pick returns the upstream responsible for the requested model and the
// model name that should be forwarded to that upstream (after alias rewrite).
// Returns ErrModelNotFound when no upstream declares the model.
func (r *Router) Pick(model string) (*Upstream, string, error) {
	if model == "" {
		return nil, "", ErrModelNotFound
	}
	u, ok := r.byModel[model]
	if !ok {
		return nil, "", fmt.Errorf("%w: %q", ErrModelNotFound, model)
	}
	target := model
	if alias, ok := u.Aliases[model]; ok {
		target = alias
	}
	return u, target, nil
}

// New builds a Router from the validated Config. Duplicate models are already
// rejected by config validation, so we trust that here.
func New(cfg *config.Config) (*Router, error) {
	if cfg == nil {
		return nil, errors.New("config is nil")
	}
	if len(cfg.Upstreams) == 0 {
		return nil, errors.New("no upstreams configured")
	}

	r := &Router{
		byModel:        make(map[string]*Upstream, len(cfg.Upstreams)*4),
		requestTimeout: cfg.Routing.Timeout,
	}

	for i := range cfg.Upstreams {
		uc := &cfg.Upstreams[i]

		parsed, err := url.Parse(uc.URL)
		if err != nil {
			return nil, fmt.Errorf("upstream %q: invalid url %q: %w", uc.Name, uc.URL, err)
		}
		if parsed.Scheme == "" || parsed.Host == "" {
			return nil, fmt.Errorf("upstream %q: url must include scheme and host, got %q", uc.Name, uc.URL)
		}

		up := &Upstream{
			Name:    uc.Name,
			URL:     parsed,
			Key:     uc.Key,
			Aliases: uc.Aliases,
		}

		for _, m := range uc.Models {
			r.byModel[m] = up
		}
		for alias := range uc.Aliases {
			r.byModel[alias] = up
		}
	}

	return r, nil
}
