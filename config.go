package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

const defaultTokenPrefix = "/default/"

// Config mirrors the original siteproxy config.json so that existing users
// can switch to the Go binary without touching their configuration.
type Config struct {
	// ProxyURL is the public base URL of the proxy site, e.g.
	// "https://your-proxy.domain.name". It is used to build proxied URLs.
	ProxyURL string `json:"proxy_url"`

	// TokenPrefix is the "password" embedded in every proxied URL, it must
	// start and end with "/". An empty value uses the upstream default token
	// "/default/"; an explicit "/" disables authentication (Go extension).
	TokenPrefix string `json:"token_prefix"`

	// LocalListenPort is the HTTP port this server listens on.
	LocalListenPort int `json:"local_listen_port"`

	// BlockedSites is a list of (sub)domains the proxy refuses to fetch.
	BlockedSites []string `json:"blocked_sites"`

	// CookieMaxAge caps every non-control Set-Cookie lifetime in seconds.
	// 1800 matches upstream v2.7.11; 0 keeps the origin's original expiry.
	CookieMaxAge int `json:"cookie_max_age_seconds"`

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
		TokenPrefix:     defaultTokenPrefix,
		LocalListenPort: 5006,
		CookieMaxAge:    1800,
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
	if v := os.Getenv("SITEPROXY_COOKIE_MAX_AGE"); v != "" {
		var age int
		if _, err := fmt.Sscanf(v, "%d", &age); err == nil && age >= 0 {
			cfg.CookieMaxAge = age
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
	if u, err := url.Parse(c.ProxyURL); err != nil || u.Host == "" || u.User != nil ||
		(u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid proxy_url %q (need an absolute http(s) URL without query/fragment)", c.ProxyURL)
	}

	c.TokenPrefix = normalizeTokenPrefix(c.TokenPrefix)
	if strings.ContainsAny(c.TokenPrefix, " \t\r\n?#") {
		return nil, fmt.Errorf("invalid token_prefix %q (whitespace, ? and # are not allowed)", c.TokenPrefix)
	}

	if c.LocalListenPort <= 0 {
		c.LocalListenPort = 5006
	}
	if c.CookieMaxAge < 0 {
		c.CookieMaxAge = 1800
	}

	blocked := make(map[string]bool, len(c.BlockedSites))
	for _, s := range c.BlockedSites {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			blocked[s] = true
		}
	}
	// Blocking is opt-in through blocked_sites. Upstream node mode does not
	// enforce this field, so there is no built-in list to merge here.
	c.BlockedSites = nil
	for s := range blocked {
		c.BlockedSites = append(c.BlockedSites, s)
	}
	return c, nil
}

// normalizeTokenPrefix ensures the prefix starts and ends with "/".
// An empty value falls back to the upstream default token "/default/"; only an
// explicit "/" disables authentication.
func normalizeTokenPrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return defaultTokenPrefix
	}
	if p == "/" {
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
	host = hostWithoutPort(strings.ToLower(host))
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

// hostWithoutPort strips a numeric port from a host (including bracketed
// IPv6 literals) so domain block entries match target URLs like
// "elcomercio.pe:443". A bare IPv6 literal is returned with its brackets.
func hostWithoutPort(host string) string {
	if strings.HasPrefix(host, "[") {
		if i := strings.IndexByte(host, ']'); i >= 0 {
			return host[:i+1]
		}
		return host
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 && strings.Count(host, ":") == 1 {
		return host[:i]
	}
	return host
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

// BasePath returns the path component of proxy_url without a trailing
// slash ("" for a root deployment). Cookie paths must include it so a proxy
// deployed under /sub still receives its own cookies.
func (c *Config) BasePath() string {
	u, err := url.Parse(c.ProxyURL)
	if err != nil {
		return ""
	}
	return strings.TrimRight(u.Path, "/")
}

// noPassword reports whether token authentication is disabled.
func (c *Config) noPassword() bool {
	return c.TokenPrefix == "/"
}
