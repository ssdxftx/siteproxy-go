package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/idna"
)

// P-31: IDN hosts must be normalized before validation and before building
// the upstream URL. Browsers usually percent-encode the Unicode label in the
// proxy path, so both raw and escaped forms are covered.
func TestIDNTargetHostIsNormalized(t *testing.T) {
	const rawHost = "例え.テスト"
	want, err := idna.ToASCII(rawHost)
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/https/" + rawHost + "/path",
		"/https/" + url.PathEscape(rawHost) + "/path",
	} {
		target := ParseTarget(path, "")
		if target == nil {
			t.Fatalf("ParseTarget(%q) returned nil", path)
		}
		if target.Host != want {
			t.Errorf("ParseTarget(%q).Host = %q, want %q", path, target.Host, want)
		}
	}

	if !isPlausibleHost(rawHost) {
		t.Errorf("isPlausibleHost(%q) = false", rawHost)
	}
	if got := UnrewriteURLWithBase("http://p.example/tok/https/"+url.PathEscape(rawHost)+"/x", "/tok/", ""); got != "https://"+want+"/x" {
		t.Errorf("UnrewriteURLWithBase IDN = %q", got)
	}
	if isPlausibleHost(url.PathEscape(rawHost) + "%2Fevil") {
		t.Error("percent-encoded slash was accepted in an IDN host")
	}
}

// P-30: the Telegram CSS branch must not rewrite @charset or url().
func TestTelegramCSSIsUntouched(t *testing.T) {
	p := &Proxy{cfg: &Config{ProxyURL: "http://proxy.test", TokenPrefix: token}}
	css := `@charset "gbk"; a{background:url(/x.png)}`
	resp := &http.Response{Header: http.Header{"Content-Type": {"text/css; charset=utf-8"}}}
	out, truncated, err := p.renderCSS(resp, strings.NewReader(css), &TargetInfo{
		Protocol: "https", Host: "web.telegram.org", Path: "/style.css",
	})
	if err != nil || truncated {
		t.Fatalf("renderCSS: err=%v truncated=%v", err, truncated)
	}
	if string(out) != css {
		t.Errorf("telegram CSS changed:\n got %q\nwant %q", out, css)
	}
}

// P-35: Google's inline SRI cleanup is HTML-only; external JavaScript must
// keep its assignments because upstream invokes the JS branch with
// skipUrlRewrite=true.
func TestGoogleExternalJSIntegrityIsPreserved(t *testing.T) {
	p := &Proxy{cfg: &Config{ProxyURL: "http://proxy.test", TokenPrefix: token}}
	js := `var s=document.createElement('script');s.integrity='sha256-abc';`
	resp := &http.Response{Header: http.Header{"Content-Type": {"application/javascript"}}}
	out, truncated, err := p.renderJS(resp, strings.NewReader(js), &TargetInfo{Host: "www.google.com"})
	if err != nil || truncated {
		t.Fatalf("renderJS: err=%v truncated=%v", err, truncated)
	}
	if !strings.Contains(string(out), "s.integrity='sha256-abc'") {
		t.Errorf("Google external JS integrity assignment was stripped:\n%s", out)
	}
}

// P-33: the runtime is served for any suffix path and must not be cached
// without revalidation.
func TestInjectedRuntimeSuffixAndCacheControl(t *testing.T) {
	up := startMockUpstream(t)
	ts := newProxyFor(t, up.URL, token)
	host := strings.TrimPrefix(up.URL, "http://")
	for _, path := range []string{
		"/foo/siteproxy-response-injected.js",
		token + "http/" + host + "/siteproxy-response-injected.js",
	} {
		resp, body := proxyGet(ts, path, nil)
		if resp == nil || resp.StatusCode != http.StatusOK {
			t.Errorf("%s status = %v, want 200", path, resp)
			continue
		}
		if !strings.Contains(string(body), "getRefererFromLocation") {
			t.Errorf("%s did not return the runtime", path)
		}
		if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Errorf("%s Cache-Control = %q, want no-store", path, cc)
		}
	}
}

// P-36/P-40: the SW must read the target context headers emitted by fetch/XHR,
// even when the client URL or Referer already supplied a context.
func TestServiceWorkerConsumesTargetHeaders(t *testing.T) {
	js := string(assetsWorker)
	for _, want := range []string{
		"siteproxy-target-host",
		"siteproxy-target-protocol",
		"siteproxy-real-referer",
		"siteReferer",
		"!url.pathname.includes(TOKEN_PREFIX)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("service worker missing %q", want)
		}
	}
	if strings.Contains(js, "contextKnown") {
		t.Error("target headers are still gated on an unknown client/Referer context")
	}
}

// P-34: a valid gzip header followed by a corrupt stream must be replayed as
// the original compressed bytes instead of becoming a 502.
func TestCorruptGzipWithValidHeaderIsReplayed(t *testing.T) {
	raw := append([]byte{0x1f, 0x8b, 0x08, 0, 0, 0, 0, 0, 0, 3}, []byte("garbage")...)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(raw)
	}))
	defer up.Close()

	ts, base := startProxy(t, up.URL)
	client := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{DisableCompression: true},
	}
	resp, err := client.Get(ts.URL + base + "/bad-gzip")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !bytes.Equal(body, raw) {
		t.Errorf("body was not replayed verbatim: got %x want %x", body, raw)
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", ce)
	}
}
