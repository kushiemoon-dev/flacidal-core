package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// poolEntry tracks availability of a single API endpoint.
type poolEntry struct {
	url         string
	blacklisted bool
	blacklistAt time.Time
}

// PoolResult contains the response from a successful endpoint request.
type PoolResult struct {
	Body     []byte
	Endpoint string
}

// EndpointPool manages a set of API endpoints with parallel racing and blacklisting.
type EndpointPool struct {
	entries      []*poolEntry
	mu           sync.Mutex
	blacklistDur time.Duration
	client       *http.Client
	logger       *LogBuffer
	userAgent    string
}

// NewEndpointPool creates a pool with the given endpoint URLs and blacklist duration.
func NewEndpointPool(urls []string, blacklistDur time.Duration) *EndpointPool {
	entries := make([]*poolEntry, len(urls))
	for i, u := range urls {
		entries[i] = &poolEntry{url: u}
	}
	return &EndpointPool{
		entries:      entries,
		blacklistDur: blacklistDur,
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: NewFallbackTransport(),
		},
		userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
	}
}

// SetClient replaces the HTTP client (e.g. for proxy support).
// Must be called before any concurrent requests begin.
func (p *EndpointPool) SetClient(client *http.Client) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.client = client
}

// GetClient returns the current HTTP client used by the pool.
func (p *EndpointPool) GetClient() *http.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.client
}

// SetLogger attaches a log buffer for endpoint rotation events.
// Must be called before any concurrent requests begin.
func (p *EndpointPool) SetLogger(logger *LogBuffer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logger = logger
}

// SetEndpoints replaces the endpoint list.
func (p *EndpointPool) SetEndpoints(urls []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entries := make([]*poolEntry, len(urls))
	for i, u := range urls {
		entries[i] = &poolEntry{url: u}
	}
	p.entries = entries
}

// AddEndpoints appends endpoints to the existing pool.
func (p *EndpointPool) AddEndpoints(urls []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, u := range urls {
		p.entries = append(p.entries, &poolEntry{url: u})
	}
}

// GetHealthy returns URLs of all non-blacklisted endpoints.
func (p *EndpointPool) GetHealthy() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var healthy []string
	for _, ep := range p.entries {
		if !ep.blacklisted || now.After(ep.blacklistAt.Add(p.blacklistDur)) {
			healthy = append(healthy, ep.url)
		}
	}
	return healthy
}

// GetAvailable returns all endpoints eligible for requests (active + expired blacklists).
func (p *EndpointPool) GetAvailable() []string {
	return p.getAvailable()
}

// Blacklist marks an endpoint as temporarily unavailable.
func (p *EndpointPool) Blacklist(rawURL string) {
	p.blacklist(rawURL)
}

// blacklist marks an endpoint as temporarily unavailable (internal).
func (p *EndpointPool) blacklist(rawURL string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ep := range p.entries {
		if ep.url == rawURL {
			ep.blacklisted = true
			ep.blacklistAt = time.Now()
			return
		}
	}
}

// getAvailable returns all endpoints eligible for requests (active first, then expired blacklists).
func (p *EndpointPool) getAvailable() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var active, expired []string
	for _, ep := range p.entries {
		if !ep.blacklisted {
			active = append(active, ep.url)
		} else if now.After(ep.blacklistAt.Add(p.blacklistDur)) {
			ep.blacklisted = false
			active = append(active, ep.url)
		} else {
			expired = append(expired, ep.url)
		}
	}
	return append(active, expired...)
}

// tryRequest performs a single GET against endpoint+path.
func (p *EndpointPool) tryRequest(ctx context.Context, endpoint, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", p.userAgent)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("server returned %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

// RaceRequest fires the request on ALL available endpoints in parallel.
// First 2xx response wins; all other in-flight requests are cancelled.
// Failed endpoints (5xx or connection error) are blacklisted.
func (p *EndpointPool) RaceRequest(ctx context.Context, path string) (*PoolResult, error) {
	endpoints := p.getAvailable()
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("no endpoints available")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		body     []byte
		endpoint string
		err      error
	}
	ch := make(chan result, len(endpoints))

	for _, ep := range endpoints {
		go func(endpoint string) {
			body, err := p.tryRequest(ctx, endpoint, path)
			ch <- result{body: body, endpoint: endpoint, err: err}
		}(ep)
	}

	var lastErr error
	var winner *PoolResult
	received := 0
	for received < len(endpoints) {
		r := <-ch
		received++
		if r.err != nil {
			// Only blacklist if it was a real failure (not context cancellation after a winner)
			if winner == nil || !isContextErr(r.err) {
				p.blacklist(r.endpoint)
				if p.logger != nil {
					p.logger.Warn(fmt.Sprintf("endpoint %s failed: %v", r.endpoint, r.err))
				}
			}
			lastErr = r.err
			continue
		}
		if winner == nil {
			// First success — cancel remaining goroutines, but keep draining to blacklist real failures
			winner = &PoolResult{Body: r.body, Endpoint: r.endpoint}
			cancel()
		}
	}

	if winner != nil {
		return winner, nil
	}

	return nil, fmt.Errorf("all endpoints failed: %v", lastErr)
}

// isContextErr returns true if the error is due to context cancellation (not a real endpoint failure).
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// SequentialRequest tries endpoints one at a time in order (active first, then expired blacklists).
// Failed endpoints are blacklisted. Returns on first success.
func (p *EndpointPool) SequentialRequest(ctx context.Context, path string) (*PoolResult, error) {
	endpoints := p.getAvailable()
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("no endpoints available")
	}

	var lastErr error
	for _, endpoint := range endpoints {
		body, err := p.tryRequest(ctx, endpoint, path)
		if err != nil {
			p.blacklist(endpoint)
			if p.logger != nil {
				p.logger.Warn(fmt.Sprintf("endpoint %s failed: %v", endpoint, err))
			}
			lastErr = err
			continue
		}
		return &PoolResult{Body: body, Endpoint: endpoint}, nil
	}

	return nil, fmt.Errorf("all endpoints failed: %v", lastErr)
}
