package main

// Tests added by the second parity audit (differences found against the
// original obfuscated v2.7.11 bundle, node mode).

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- rewrite.go: inline-script location assignment rewrites ------------------

func TestInlineScriptLocationRewrites(t *testing.T) {
	publicPrefix := "http://p.example" + token
	base := publicPrefix + "http/mock.host/"
	html := `<html><head><script>
window.location.href = "/goto";
window.location = "/land";
if (window.location == a) { }
location.href = "/bare";
var u = "https://api.example.com/endpoint";
var my_location = 1;
var r = /location/;
</script></head></html>`
	got := rewriteHTML(html, publicPrefix, base, "p.example",
		renderInlineBootstrap(newTestConfig(0), "http", "mock.host")+injectedScriptTag)
	// Assignments rewritten with whitespace preserved (upstream shape).
	if !strings.Contains(got, "window.___location.href = \"/goto\";") {
		t.Errorf("href assignment not rewritten (space-preserving):\n%s", got)
	}
	if !strings.Contains(got, "window.___location = \"/land\";") {
		t.Errorf("location assignment not rewritten:\n%s", got)
	}
	// Identifier rewrites also cover comparisons and bare location, while
	// strings/regex literals and longer identifiers stay untouched.
	if !strings.Contains(got, "if (window.___location == a) { }") {
		t.Errorf("comparison identifier not rewritten:\n%s", got)
	}
	if !strings.Contains(got, "___location.href = \"/bare\";") {
		t.Errorf("bare location not rewritten:\n%s", got)
	}
	if !strings.Contains(got, `var u = "https://api.example.com/endpoint";`) {
		t.Errorf("string literal must stay untouched:\n%s", got)
	}
	if strings.Contains(got, "my___location") {
		t.Errorf("longer identifier must not be rewritten:\n%s", got)
	}
	if !strings.Contains(got, "var r = /location/;") {
		t.Errorf("regex literal corrupted:\n%s", got)
	}
}

// --- rewrite.go: external JS identifier rewrites -----------------------------

func TestJSFileLocationRewrites(t *testing.T) {
	in := "window.location.href = \"/next\";\n" +
		"window.location = c;\n" +
		"if (window.location == a) { }\n" +
		"location.href = \"/bare\";\n"
	got := rewriteJSFile(in)
	// The identifier scanner preserves the original spacing and also rewrites
	// bare location references.
	if !strings.Contains(got, "window.___location.href = \"/next\";") {
		t.Errorf("href rewrite mismatch:\n%s", got)
	}
	if !strings.Contains(got, "window.___location = c;") {
		t.Errorf("plain location rewrite mismatch:\n%s", got)
	}
	if !strings.Contains(got, "if (window.___location == a) { }") {
		t.Errorf("comparison rewrite mismatch:\n%s", got)
	}
	if !strings.Contains(got, "___location.href = \"/bare\";") {
		t.Errorf("bare location not rewritten:\n%s", got)
	}
}

// --- rewrite.go: injection includes inline bootstrap + external tag ----------

func TestInjectionSnippet(t *testing.T) {
	cfg := newTestConfig(0)
	inject := renderInlineBootstrap(cfg, "http", "mock.host") + injectedScriptTag
	if !strings.Contains(inject, "proxy_real_host = 'mock.host'") {
		t.Errorf("bootstrap must bake the site host:\n%s", inject)
	}
	if !strings.Contains(inject, injectedScriptTag) {
		t.Errorf("external tag missing")
	}
	html := "<html><head></head></html>"
	got := injectScripts(html, inject)
	if !strings.Contains(got, "<head>"+charsetMetaSnippet+"<script>") {
		t.Errorf("injection must land right after <head>:\n%s", got)
	}
	if !strings.Contains(got, injectedScriptTag) {
		t.Errorf("external tag missing in output:\n%s", got)
	}
}

// --- proxy.go: cookie oversize 431 -------------------------------------------

func TestCookieTooLarge431(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)
	var b strings.Builder
	for i := 0; i < 1200; i++ {
		fmt.Fprintf(&b, "c%d=x;", i)
	}
	resp, _ := proxyGet(ts, base+"/echo", map[string]string{"Cookie": b.String()})
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("want 431, got %d", resp.StatusCode)
	}
	sc := resp.Header.Values("Set-Cookie")
	expired := 0
	for _, c := range sc {
		if strings.Contains(c, "Expires=Thu, 01 Jan 1970") {
			expired++
		}
	}
	if expired != 12 { // 2 cookies × 6 Domain/Path variants
		t.Errorf("want 12 expired-cookie headers, got %d: %v", expired, sc)
	}
}

// --- server.go: out-of-scope repair ------------------------------------------

func TestRepairOutOfScopeViaReferer(t *testing.T) {
	up := startMockUpstream(t)
	ts, _ := startProxy(t, up.URL)
	// cfg.ProxyURL is the upstream URL in tests, so the referer must carry
	// it as the proxy origin (that is what startProxy configures).
	host := strings.TrimPrefix(up.URL, "http://")
	referer := up.URL + token + "http/" + host + "/some/page"
	_, body := proxyGet(ts, "/echo?q=1", map[string]string{"Referer": referer})
	if body == nil {
		t.Fatal("no body")
	}
	if !strings.Contains(string(body), `"requrl":"/echo?q=1"`) {
		t.Errorf("repaired request did not reach upstream with original path: %s", body)
	}
}

func TestRepairOutOfScopeViaControlCookies(t *testing.T) {
	up := startMockUpstream(t)
	ts, _ := startProxy(t, up.URL)
	host := strings.TrimPrefix(up.URL, "http://")
	cookie := "proxy_real_protocol=http; proxy_real_host=" + host
	_, body := proxyGet(ts, "/echo", map[string]string{"Cookie": cookie})
	if body == nil {
		t.Fatal("no body")
	}
	if !strings.Contains(string(body), `"requrl":"/echo"`) {
		t.Errorf("cookie-based repair failed: %s", body)
	}
}

func TestRepairNotAppliedWithoutContext(t *testing.T) {
	up := startMockUpstream(t)
	ts, _ := startProxy(t, up.URL)
	resp, _ := proxyGet(ts, "/unknown/path", nil)
	if resp == nil {
		t.Fatal("no response")
	}
	// Without referer/cookies the request cannot be attributed: homepage.
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Errorf("expected homepage fallback, got %s", resp.Header.Get("Content-Type"))
	}
}

// --- config.go: path-style block entries --------------------------------------

func TestBlockPathEntries(t *testing.T) {
	up := startMockUpstream(t)
	cfg := newTestConfig(0)
	cfg.ProxyURL = up.URL
	cfg.BlockedSites = []string{"telegram.org/service_worker.js"}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	host := strings.TrimPrefix(up.URL, "http://")
	resp, _ := proxyGet(ts, token+"http/"+host+"/telegram.org/service_worker.js", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403 for path-blocked target, got %d", resp.StatusCode)
	}
}

func TestIsBlockedURLSemantics(t *testing.T) {
	cfg := newTestConfig(0)
	cfg.BlockedSites = []string{"telegram.org/service_worker.js", "bad.example"}
	if !cfg.IsBlockedURL("http://x.com/telegram.org/service_worker.js") {
		t.Error("path entry must match substring of URL")
	}
	if cfg.IsBlockedURL("http://telegram.org/other.js") {
		t.Error("path entry must not match unrelated URL")
	}
	if !cfg.IsBlocked("sub.bad.example") {
		t.Error("domain entry must match subdomains")
	}
	if cfg.IsBlocked("notbad.example") {
		t.Error("domain entry must not match suffix-impostors")
	}
}

// --- headers.go: prefix-based request header filtering ------------------------

func TestHeaderFilterPrefixes(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)
	hdr := map[string]string{
		"Cf-Foo":                    "leak",
		"Cf-Connecting-Ip":          "1.2.3.4",
		"X-Forwarded-Port":          "8443",
		"X-Forwarded-For":           "9.9.9.9",
		"Siteproxy-Whatever":        "internal",
		"X-Siteproxy-Body-Headers":  "smuggle",
		"X-Client-Ip":               "5.6.7.8",
		"Fastly-Client-Ip":          "5.6.7.9",
		"Fly-Client-Ip":             "5.6.7.10",
		"Upgrade-Insecure-Requests": "1",
		"X-Keep":                    "fine",
	}
	_, body := proxyGet(ts, base+"/echo-headers", hdr)
	if body == nil {
		t.Fatal("no body")
	}
	s := string(body)
	for _, banned := range []string{"cf-foo", "cf-connecting-ip", "x-forwarded-port",
		"x-forwarded-for", "siteproxy-whatever", "x-siteproxy-body-headers",
		"x-client-ip", "fastly-client-ip", "fly-client-ip"} {
		if strings.Contains(s, `"`+banned+`"`) {
			t.Errorf("header %q leaked upstream: %s", banned, s)
		}
	}
	if !strings.Contains(s, `"upgrade-insecure-requests":"1"`) {
		t.Errorf("Upgrade-Insecure-Requests must be forwarded (upstream behaviour): %s", s)
	}
	if !strings.Contains(s, `"x-keep":"fine"`) {
		t.Errorf("ordinary header dropped: %s", s)
	}
}

// --- headers.go: response-side filtering --------------------------------------

func TestResponseHeaderFiltering(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)
	resp, _ := proxyGet(ts, base+"/resp-headers", nil)
	if resp == nil {
		t.Fatal("no response")
	}
	for _, banned := range []string{"Cf-Ray", "X-Forwarded-Foo", "Siteproxy-Internal", "Via"} {
		if resp.Header.Get(banned) != "" {
			t.Errorf("response header %q not stripped", banned)
		}
	}
}
