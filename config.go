package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Config mirrors the original siteproxy config.json so that existing users
// can switch to the Go binary without touching their configuration.
type Config struct {
	// ProxyURL is the public base URL of the proxy site, e.g.
	// "https://your-proxy.domain.name". It is used to build proxied URLs.
	ProxyURL string `json:"proxy_url"`

	// TokenPrefix is the "password" embedded in every proxied URL, it must
	// start and end with "/". Empty string or "/" means no password.
	TokenPrefix string `json:"token_prefix"`

	// LocalListenPort is the HTTP port this server listens on.
	LocalListenPort int `json:"local_listen_port"`

	// BlockedSites is a list of (sub)domains the proxy refuses to fetch.
	BlockedSites []string `json:"blocked_sites"`

	// Description is informational only, kept for config.json compatibility.
	Description string `json:"description"`
}

// LoadConfig reads config.json (when present), then applies environment
// variable overrides:
//
//	SITEPROXY_PROXY_URL          -> ProxyURL
//	SITEPROXY_TOKEN_PREFIX       -> TokenPrefix
//	SITEPROXY_LOCAL_LISTEN_PORT  -> LocalListenPort
//	SITEPROXY_BLOCKED_SITES      -> BlockedSites (comma separated)
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{
		ProxyURL:        "http://localhost:5006",
		TokenPrefix:     "/user22334455/",
		LocalListenPort: 5006,
	}

	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	// Environment overrides (handy for docker / systemd).
	if v := os.Getenv("SITEPROXY_PROXY_URL"); v != "" {
		cfg.ProxyURL = v
	}
	if v := os.Getenv("SITEPROXY_TOKEN_PREFIX"); v != "" {
		cfg.TokenPrefix = v
	}
	if v := os.Getenv("SITEPROXY_LOCAL_LISTEN_PORT"); v != "" {
		var port int
		if _, err := fmt.Sscanf(v, "%d", &port); err == nil && port > 0 {
			cfg.LocalListenPort = port
		}
	}
	if v := os.Getenv("SITEPROXY_BLOCKED_SITES"); v != "" {
		cfg.BlockedSites = nil
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				cfg.BlockedSites = append(cfg.BlockedSites, s)
			}
		}
	}

	return cfg.normalize()
}

// normalize validates and canonicalises the configuration.
func (c *Config) normalize() (*Config, error) {
	c.ProxyURL = strings.TrimRight(c.ProxyURL, "/")
	if c.ProxyURL == "" {
		c.ProxyURL = "http://localhost:5006"
	}
	if u, err := url.Parse(c.ProxyURL); err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid proxy_url %q", c.ProxyURL)
	}

	c.TokenPrefix = normalizeTokenPrefix(c.TokenPrefix)

	if c.LocalListenPort <= 0 {
		c.LocalListenPort = 5006
	}

	blocked := make(map[string]bool, len(c.BlockedSites))
	for _, s := range c.BlockedSites {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			blocked[s] = true
		}
	}
	// Built-in block list carried over from upstream siteproxy v2.7.11
	// (default block_urls). Entries containing "/" match anywhere in the
	// target URL; plain domain entries match the host or its subdomains.
	for _, s := range []string{
		"telegram.org/service_worker.js",
		"elcomercio.pe",
		"exchangebank.com",
		"www.visa.com.sg",
		"netsuite.com",
	} {
		blocked[s] = true
	}
	c.BlockedSites = nil
	for s := range blocked {
		c.BlockedSites = append(c.BlockedSites, s)
	}
	return c, nil
}

// normalizeTokenPrefix ensures the prefix starts and ends with "/".
// "/", "" and "///" all mean "no password".
func normalizeTokenPrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// IsBlocked reports whether host matches (or is a subdomain of) any blocked
// domain entry. Path-style entries (containing "/") are matched by
// IsBlockedURL instead, mirroring the upstream substring semantics.
func (c *Config) IsBlocked(host string) bool {
	host = strings.ToLower(host)
	host = strings.TrimSuffix(host, ".")
	for _, b := range c.BlockedSites {
		if strings.Contains(b, "/") {
			continue
		}
		if host == b || strings.HasSuffix(host, "."+b) {
			return true
		}
	}
	return false
}

// IsBlockedURL checks path-style block entries (e.g.
// "telegram.org/service_worker.js") against the full target URL, mirroring
// the upstream `extractedUrl.includes(entry)` check.
func (c *Config) IsBlockedURL(fullURL string) bool {
	fullURL = strings.ToLower(fullURL)
	for _, b := range c.BlockedSites {
		if strings.Contains(b, "/") && strings.Contains(fullURL, b) {
			return true
		}
	}
	return false
}

// noPassword reports whether token authentication is disabled.
func (c *Config) noPassword() bool {
	return c.TokenPrefix == "/"
}
