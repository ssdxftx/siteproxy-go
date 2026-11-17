package main

import (
	"net/http"
	"strings"
	"testing"
)

// P-01: telegram.org skips URL rewriting, but identifier rewriting and
// script/link integrity cleanup still run.
func TestTelegramHostSkipsURLRewrite(t *testing.T) {
	html := `<html><head></head><body>` +
		`<a href="https://example.com/a?b=1#c">abs</a>` +
		`<img src="/logo.png" srcset="//cdn.example.com/a.png 1x, /b.png 2x">` +
		`<script>window.location.href = "/dashboard";</script>` +
		`<script src="/app.js" integrity="sha384-x"></script>` +
		`</body></html>`

	pp := "http://proxy.test" + token
	base := pp + "https/telegram.org/web"
	out := rewriteHTMLForHost(html, pp, base, "proxy.test", "", true, "web.telegram.org")

	if !strings.Contains(out, `href="https://example.com/a?b=1#c"`) {
		t.Errorf("telegram URL was rewritten:\n%s", out)
	}
	if strings.Contains(out, "proxy.test"+token+"https/cdn.example.com") {
		t.Errorf("telegram srcset URL was rewritten:\n%s", out)
	}
	if strings.Contains(out, "integrity=") {
		t.Errorf("script integrity attribute was not removed:\n%s", out)
	}
	if !strings.Contains(out, "window.___location.href") {
		t.Errorf("identifier rewriting did not run:\n%s", out)
	}
}

// A normal host must still rewrite the same document.
func TestNonExemptHostStillRewritesURLs(t *testing.T) {
	html := `<html><head></head><body><a href="https://example.com/a?b=1#c">abs</a></body></html>`
	pp := "http://proxy.test" + token
	base := pp + "https/example.com/web"
	out := rewriteHTMLForHost(html, pp, base, "proxy.test", "", true, "example.com")

	want := `href="` + pp + `https/example.com/a?b=1#c"`
	if !strings.Contains(out, want) {
		t.Errorf("normal host URL was not rewritten, want %q in:\n%s", want, out)
	}
}

// P-02: google.com inline integrity assignments are removed before script
// extraction; other hosts keep them.
func TestGoogleIntegrityAssignmentStripped(t *testing.T) {
	html := `<html><head><script>var s=document.createElement('script');s.integrity='sha256-abc';</script></head><body></body></html>`
	pp := "http://proxy.test" + token
	base := pp + "https/google.com/search"

	google := rewriteHTMLForHost(html, pp, base, "proxy.test", "", true, "www.google.com")
	if strings.Contains(google, "integrity='sha256-abc'") {
		t.Errorf("google inline integrity assignment survived:\n%s", google)
	}

	other := rewriteHTMLForHost(html, pp, base, "proxy.test", "", true, "example.com")
	if !strings.Contains(other, "integrity='sha256-abc'") {
		t.Errorf("non-google inline integrity assignment was removed:\n%s", other)
	}
}

// P-04: the first proxy-form URL in a query string is restored.
func TestUnrewriteQueryProxyURLs(t *testing.T) {
	pp := "http://proxy.test" + token
	in := "u=" + pp + "https/example.com/x&z=1"
	want := "u=https://example.com/x&z=1"
	if got := unrewriteQueryProxyURLs(in, pp); got != want {
		t.Fatalf("unrewriteQueryProxyURLs(%q) = %q, want %q", in, got, want)
	}
	if got := unrewriteQueryProxyURLs("q=plain", pp); got != "q=plain" {
		t.Fatalf("plain query changed: %q", got)
	}
	// Upstream String.replace has no /g flag: only the first match changes.
	in = "a=" + pp + "https/a.test/x&b=" + pp + "https/b.test/y&c=" + pp + "http/c.test/z"
	want = "a=https://a.test/x&b=" + pp + "https/b.test/y&c=" + pp + "http/c.test/z"
	if got := unrewriteQueryProxyURLs(in, pp); got != want {
		t.Fatalf("only first query URL must be restored:\n got %q\nwant %q", got, want)
	}
}

func TestQueryProxyURLReachesUpstreamUnrewritten(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)
	pp := up.URL + token
	resp, body := proxyGet(ts, base+"/echo?u="+pp+"https/example.com/x", nil)
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%v body=%s", resp, body)
	}
	if !strings.Contains(string(body), `"requrl":"/echo?u=https://example.com/x"`) {
		t.Errorf("query URL was not un-rewritten upstream: %s", body)
	}
}

// P-03: a token prefix in the middle of the path is used as an anchor.
func TestMidPathTokenRepair(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)

	resp, body := proxyGet(ts, "/foo"+base+"/echo?q=1", nil)
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("mid-path token status=%v body=%s", resp, body)
	}
	if !strings.Contains(string(body), `"requrl":"/echo?q=1"`) {
		t.Errorf("mid-path token request reached wrong upstream path: %s", body)
	}

	// The upstream also repairs a second token after the leading one.
	resp, body = proxyGet(ts, base+"/foo"+base+"/echo", nil)
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("repeated token status=%v body=%s", resp, body)
	}
	if !strings.Contains(string(body), `"requrl":"/echo"`) {
		t.Errorf("repeated token request reached wrong upstream path: %s", body)
	}

	// A bare host after the mid-path token is NOT repaired by upstream.
	resp, body = proxyGet(ts, "/foo"+base, nil)
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("host-only mid-token must not be proxied: status=%v body=%s", resp, body)
	}
}
