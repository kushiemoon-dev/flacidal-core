package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// dohEndpoints are Cloudflare and Google DoH servers used as fallback resolvers.
var dohEndpoints = []string{
	"https://1.1.1.1/dns-query",
	"https://8.8.8.8/dns-query",
}

// dohResolve resolves host using DNS-over-HTTPS, trying each endpoint in order.
func dohResolve(ctx context.Context, host string) ([]string, error) {
	dohClient := &http.Client{Timeout: 5 * time.Second}
	for _, endpoint := range dohEndpoints {
		ips, err := dohLookup(ctx, dohClient, endpoint, host)
		if err == nil && len(ips) > 0 {
			return ips, nil
		}
	}
	return nil, fmt.Errorf("DoH resolution failed for %s", host)
}

func dohLookup(ctx context.Context, client *http.Client, endpoint, host string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s?name=%s&type=A", endpoint, host), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/dns-json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		} `json:"Answer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	var ips []string
	for _, a := range result.Answer {
		if a.Type == 1 { // A record
			ip := net.ParseIP(a.Data)
			if ip != nil && !ip.Equal(net.IPv4zero) && !ip.Equal(net.IPv4(0, 0, 0, 0)) {
				ips = append(ips, a.Data)
			}
		}
	}
	return ips, nil
}

// NewFallbackDialer returns a DialContext function that uses the system resolver
// but falls back to DoH when the system returns a sinkholed address (0.0.0.0).
func NewFallbackDialer() func(ctx context.Context, network, addr string) (net.Conn, error) {
	defaultDialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return defaultDialer.DialContext(ctx, network, addr)
		}

		// Try system DNS first
		addrs, err := net.DefaultResolver.LookupHost(ctx, host)
		needsFallback := err != nil
		if !needsFallback {
			for _, a := range addrs {
				ip := net.ParseIP(a)
				// Sinkholed if all resolved IPs are 0.0.0.0
				if ip != nil && (ip.Equal(net.IPv4zero) || ip.String() == "0.0.0.0") {
					needsFallback = true
				} else {
					needsFallback = false
					break
				}
			}
		}

		if !needsFallback {
			// System DNS is fine — connect normally
			return defaultDialer.DialContext(ctx, network, addr)
		}

		// Fall back to DoH
		ips, dohErr := dohResolve(ctx, host)
		if dohErr != nil {
			if err != nil {
				return nil, fmt.Errorf("system DNS: %w; DoH fallback: %v", err, dohErr)
			}
			return nil, dohErr
		}

		// Try each DoH-resolved IP until one connects
		var lastErr error
		for _, ip := range ips {
			conn, connErr := defaultDialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
			if connErr == nil {
				return conn, nil
			}
			lastErr = connErr
		}
		return nil, fmt.Errorf("all DoH IPs failed for %s: %w", host, lastErr)
	}
}

// NewFallbackTransport returns an http.Transport with the DoH fallback dialer.
func NewFallbackTransport() *http.Transport {
	return &http.Transport{
		DialContext:         NewFallbackDialer(),
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
}
