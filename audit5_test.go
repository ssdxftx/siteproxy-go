package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newConfiguredProxy(t *testing.T, upstreamURL string, mutate func(*Config)) *httptest.Server {
	t.Helper()
	cfg := newTestConfig(0)
	cfg.ProxyURL = upstreamURL
	if mutate != nil {
		mutate(cfg)
	}
	normalized, err := cfg.normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	srv, err := NewServer(normalized)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// P-21: token_prefix "", "/" and "//" are the upstream shared password
// "/default/", not a Go no-auth mode. Invalid paths redirect to the public
// entry, while /default/http/... still proxies normally.
func TestDefaultPasswordSemantics(t *testing.T) {
	up := startMockUpstream(t)
	host := strings.TrimPrefix(up.URL, "http://")

	ts := newConfiguredProxy(t, up.URL, func(cfg *Config) { cfg.TokenPrefix = "/" })
	resp, _ := proxyGetNoRedirect(t, ts, "/")
	if resp == nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("default-password root status = %v, want 302", resp)
	}
	wantLoc := up.URL + "/default/https/www.netptop.com"
	if got := resp.Header.Get("Location"); got != wantLoc {
		t.Errorf("root Location = %q, want %q", got, wantLoc)
	}

	resp, _ = proxyGetNoRedirect(t, ts, "/default/")
	if resp == nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("default-password token root status = %v, want 302", resp)
	}

	resp, body := proxyGet(ts, "/default/http/"+host+"/binary", nil)
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("default-password proxied request status = %v body=%q", resp, body)
	}
	if string(body) != "\x00\x01\x02\x03\xff\xfe" {
		t.Errorf("default-password proxied body = %q", body)
	}
}

func TestDisableAuthIsExplicit(t *testing.T) {
	up := startMockUpstream(t)
	host := strings.TrimPrefix(up.URL, "http://")

	ts := newConfiguredProxy(t, up.URL, func(cfg *Config) {
		cfg.TokenPrefix = "/"
		cfg.DisableAuth = true
	})
	if resp, body := proxyGet(ts, "/", nil); resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("disable_auth homepage status = %v body=%q", resp, body)
	}
	resp, body := proxyGet(ts, "/http/"+host+"/binary", nil)
	if resp == nil || resp.StatusCode != http.StatusOK || string(body) != "\x00\x01\x02\x03\xff\xfe" {
		t.Fatalf("disable_auth proxy status = %v body=%q", resp, body)
	}
}

// P-22: Permissions-Policy-Report-Only must not leak the upstream policy.
func TestPermissionsPolicyReportOnlyStripped(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.Header().Set("Permissions-Policy", "geolocation=()")
		w.Header().Set("Permissions-Policy-Report-Only", "geolocation=()")
		fmt.Fprint(w, "<html><body>ok</body></html>")
	}))
	defer up.Close()

	ts, base := startProxy(t, up.URL)
	resp, _ := proxyGet(ts, base+"/x", nil)
	if resp == nil {
		t.Fatal("no response")
	}
	for _, h := range []string{"Permissions-Policy", "Permissions-Policy-Report-Only"} {
		if got := resp.Header.Get(h); got != "" {
			t.Errorf("%s leaked: %q", h, got)
		}
	}
}

// P-28: 204/304 and redirects must not retain representation headers.
func TestNoBodyAndRedirectHeadersAreStripped(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/304":
			w.Header().Set("Content-Type", "text/html; charset=UTF-8")
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Length", "123")
			w.WriteHeader(http.StatusNotModified)
		case "/redirect":
			w.Header().Set("Location", "/next")
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Length", "123")
			w.WriteHeader(http.StatusFound)
		default:
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><body>ok</body></html>")
		}
	}))
	defer up.Close()

	ts, base := startProxy(t, up.URL)
	host := strings.TrimPrefix(up.URL, "http://")
	resp, _ := proxyGetNoRedirect(t, ts, token+"http/"+host+"/304")
	if resp == nil || resp.StatusCode != http.StatusNotModified {
		t.Fatalf("304 status = %v", resp)
	}
	for _, h := range []string{"Content-Length", "Content-Encoding", "Transfer-Encoding"} {
		if got := resp.Header.Get(h); got != "" {
			t.Errorf("304 %s leaked: %q", h, got)
		}
	}

	resp, _ = proxyGetNoRedirect(t, ts, token+"http/"+host+"/redirect")
	if resp == nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("redirect status = %v", resp)
	}
	for _, h := range []string{"Content-Encoding", "Transfer-Encoding"} {
		if got := resp.Header.Get(h); got != "" {
			t.Errorf("redirect %s leaked: %q", h, got)
		}
	}
	// net/http synthesises Content-Length: 0 for a bodyless 3xx even after
	// the origin value was removed; anything else means the origin length
	// survived.
	if got := resp.Header.Get("Content-Length"); got != "" && got != "0" {
		t.Errorf("redirect Content-Length leaked: %q", got)
	}
	_ = base
}

// P-23/P-24/P-25/P-27: the public runtime exposes the upstream compatibility
// surface and does not bake in a token.
func TestInjectedCompatibilitySurface(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)
	_ = base
	resp, body := proxyGet(ts, "/siteproxy-response-injected.js", nil)
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("injected script status = %v", resp)
	}
	js := string(body)
	for _, want := range []string{
		"getRefererFromLocation",
		"getHostFromProxyPrefixedURL",
		"getPathnameFromProxyPrefixedURL",
		"getProtocolFromProxyPrefixedURL",
		"setProtocolFromProxyPrefixedURL",
		"siteproxyAttributeChanged",
		"debouncedAttributeChanged",
		"siteproxyRegReplacementFallback",
		"removeProxyPrefixFallback",
		"siteproxy-target-protocol",
		"siteproxy-target-host",
		"siteproxy-real-referer",
		"siteproxy-window-location-pathname",
		"topBarLastShown",
		"History.prototype.pushState",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("injected runtime missing %q", want)
		}
	}
}
