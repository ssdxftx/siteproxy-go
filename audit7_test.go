package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/idna"
)

// P-38: the service worker endpoint requires a real target host. Missing or
// invalid hosts must 404 like upstream instead of falling back to a Referer.
func TestServiceWorkerRequiresValidTargetHost(t *testing.T) {
	up := startMockUpstream(t)
	ts := newProxyFor(t, up.URL, token)
	base := "/siteproxy_service_worker.js?token_prefix=" + url.QueryEscape(token)

	for _, tc := range []struct {
		name string
		path string
		hdr  map[string]string
	}{
		{
			name: "missing host",
			path: base + "&proxy_real_protocol=https",
		},
		{
			name: "invalid host",
			path: base + "&proxy_real_protocol=https&proxy_real_host=bad%2Fhost",
		},
		{
			name: "referer must not backfill",
			path: base + "&proxy_real_protocol=https",
			hdr:  map[string]string{"Referer": ts.URL + token + "http/t.example/page"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := proxyGet(ts, tc.path, tc.hdr)
			if resp == nil || resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %v, want 404", resp)
			}
		})
	}

	resp, body := proxyGet(ts, base+"&proxy_real_protocol=https&proxy_real_host=t.example", nil)
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("valid host status = %v, want 200", resp)
	}
	if !strings.Contains(string(body), `proxy_real_host: "t.example"`) {
		t.Errorf("service worker did not embed the validated host")
	}
}

// P-39: blocked_sites entries must be canonicalised with IDNA just like the
// target host, otherwise a Unicode configuration entry never matches.
func TestBlockedSitesUnicodeIsNormalized(t *testing.T) {
	const rawHost = "例え.テスト"
	asciiHost, err := idna.ToASCII(rawHost)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := (&Config{
		ProxyURL:    "http://proxy.test",
		TokenPrefix: "/t/",
		BlockedSites: []string{
			rawHost,
			"ELCOMERCIO.PE.",
			"path.example/x",
		},
	}).normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}

	for _, host := range []string{
		asciiHost,
		"cdn." + asciiHost,
		rawHost,
		"elcomercio.pe:443",
		"www.elcomercio.pe",
	} {
		if !cfg.IsBlocked(host) {
			t.Errorf("IsBlocked(%q) = false", host)
		}
	}
	if cfg.IsBlocked(asciiHost + ".evil") {
		t.Errorf("IsBlocked(%q) matched a suffix impostor", asciiHost+".evil")
	}
	if !cfg.IsBlockedURL("https://x.example/path.example/x") {
		t.Error("path-style blocked entry was not preserved")
	}
}
