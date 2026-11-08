package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func newProxyFor(t *testing.T, upstreamURL, tokenPrefix string) *httptest.Server {
	t.Helper()
	cfg := newTestConfig(0)
	cfg.ProxyURL = upstreamURL
	cfg.TokenPrefix = tokenPrefix
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

func TestDefaultTokenMatchesUpstream(t *testing.T) {
	if got := normalizeTokenPrefix(""); got != "/user22334455/" {
		t.Fatalf("empty token_prefix = %q, want upstream default /user22334455/", got)
	}
	if got := normalizeTokenPrefix("/"); got != "/" {
		t.Fatalf("explicit / = %q", got)
	}
}

func TestPublicInjectedScriptDoesNotLeakToken(t *testing.T) {
	up := startMockUpstream(t)
	ts := newProxyFor(t, up.URL, "/s3cretPass/")
	resp, body := proxyGet(ts, "/siteproxy-response-injected.js", nil)
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("injected script status = %v", resp)
	}
	if strings.Contains(string(body), "s3cretPass") {
		t.Fatal("public injected script contains token_prefix")
	}
	if !strings.Contains(string(body), "window.config_token_prefix") {
		t.Fatal("injected script no longer reads the inline config globals")
	}
}

func TestServiceWorkerRequiresRegistrationToken(t *testing.T) {
	up := startMockUpstream(t)
	ts := newProxyFor(t, up.URL, "/s3cretPass/")
	if resp, _ := proxyGet(ts, "/siteproxy_service_worker.js?proxy_real_protocol=https&proxy_real_host=t.example", nil); resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("anonymous SW fetch status = %v, want 404", resp)
	}
	q := "/siteproxy_service_worker.js?proxy_real_protocol=https&proxy_real_host=t.example&token_prefix=" + url.QueryEscape("/s3cretPass/")
	resp, body := proxyGet(ts, q, nil)
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated SW fetch status = %v", resp)
	}
	if !strings.Contains(string(body), "/s3cretPass/") {
		t.Fatal("SW registration did not receive its token context")
	}
}

func TestHomepageRequiresTokenScope(t *testing.T) {
	up := startMockUpstream(t)
	ts := newProxyFor(t, up.URL, "/s3cretPass/")
	if resp, body := proxyGet(ts, "/", nil); resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("public homepage status = %v body=%q, want 404", resp, body)
	}
	resp, body := proxyGet(ts, "/s3cretPass/", nil)
	if resp == nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "<title>Siteproxy</title>") {
		t.Fatalf("token-scoped homepage status = %v", resp)
	}
}

func TestFaviconUsesOutOfScopeRepair(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			fmt.Fprint(w, "real-icon")
			return
		}
		fmt.Fprint(w, "<html><body>page</body></html>")
	}))
	defer up.Close()
	ts := newProxyFor(t, up.URL, token)
	if resp, _ := proxyGet(ts, "/favicon.ico", nil); resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("context-less favicon status = %v, want 404", resp)
	}
	host := strings.TrimPrefix(up.URL, "http://")
	ref := up.URL + token + "http/" + host + "/page"
	resp, body := proxyGet(ts, "/favicon.ico", map[string]string{"Referer": ref})
	if resp == nil || resp.StatusCode != http.StatusOK || string(body) != "real-icon" {
		t.Fatalf("favicon repair status=%v body=%q", resp, body)
	}
	// A Referer for the target root carrying a query string must parse the
	// same way (the old raw string split put "host?x=1" into the host).
	rootRef := up.URL + token + "http/" + host + "?x=1"
	resp, body = proxyGet(ts, "/favicon.ico", map[string]string{"Referer": rootRef})
	if resp == nil || resp.StatusCode != http.StatusOK || string(body) != "real-icon" {
		t.Fatalf("favicon repair with root query status=%v body=%q", resp, body)
	}
}

func TestEmptyAnd204DoNotGainControlCookies(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/empty":
			w.Header().Set("Content-Type", "text/html")
		case "/204":
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><body>page</body></html>")
		}
	}))
	defer up.Close()
	ts := newProxyFor(t, up.URL, token)
	host := strings.TrimPrefix(up.URL, "http://")
	for _, p := range []string{"/empty", "/204"} {
		resp, _ := proxyGet(ts, token+"http/"+host+p, nil)
		for _, c := range resp.Header.Values("Set-Cookie") {
			if strings.HasPrefix(c, "proxy_real_") {
				t.Errorf("%s unexpectedly got control cookie %q", p, c)
			}
		}
	}
	resp, _ := proxyGet(ts, token+"http/"+host+"/html", nil)
	found := false
	for _, c := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(c, "proxy_real_host=") {
			found = true
		}
	}
	if !found {
		t.Error("normal rewritten HTML lost its control cookie")
	}
}

func TestLateMetaCharsetIsDecoded(t *testing.T) {
	gbk := []byte{0xd6, 0xd0, 0xce, 0xc4} // 中文
	raw := append([]byte(strings.Repeat(" ", 5000)+`<meta charset="gbk"><p>`), gbk...)
	raw = append(raw, []byte("</p>")...)
	got, used := decodeHTMLBody(raw, "text/html")
	if used == "" || !strings.Contains(got, "中文") {
		t.Fatalf("late meta charset not decoded: used=%q got=%q", used, got)
	}
}

func TestCSSCharsetIsDecodedAndRewritten(t *testing.T) {
	gbk := []byte{0xd6, 0xd0, 0xce, 0xc4} // 中文
	raw := append([]byte(`@charset "gbk";.x{content:"`), gbk...)
	raw = append(raw, []byte(`"}`)...)
	txt, used := decodeTextBody(raw, "text/css")
	if used == "" || !bytes.Contains(txt, []byte("中文")) {
		t.Fatalf("CSS @charset not decoded: used=%q body=%q", used, txt)
	}
	out := string(rewriteCSSCharset(txt))
	if !strings.HasPrefix(out, `@charset "UTF-8";`) || !strings.Contains(out, "中文") {
		t.Fatalf("CSS @charset not rewritten: %q", out)
	}
}

func TestUnknownEncodingInChainDoesNotConsumeBody(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte("<html>body</html>"))
	_ = zw.Close()
	data := buf.Bytes()
	r := bytes.NewReader(data)
	if _, ok := newDecoderChain("zstd, gzip", r); ok {
		t.Fatal("unknown coding chain unexpectedly accepted")
	}
	if r.Len() != len(data) {
		t.Fatalf("body consumed before fallback: %d/%d bytes left", r.Len(), len(data))
	}
}

func TestRewriteURLStripsUserinfo(t *testing.T) {
	pp := "https://p.example" + token
	got := RewriteURL("http://u:p@example.com/x", pp, "p.example")
	if strings.Contains(got, "u:p@") || !strings.Contains(got, "/http/example.com/x") {
		t.Fatalf("userinfo not stripped: %q", got)
	}
	got = RewriteWithBase("//u:p@example.com/x", pp, pp+"http/t.example/page", "p.example")
	if strings.Contains(got, "u:p@") || !strings.Contains(got, "/https/example.com/x") {
		t.Fatalf("protocol-relative userinfo not stripped: %q", got)
	}
}

func TestRelativeDotDotResolvesInTargetSpace(t *testing.T) {
	pp := "https://p.example" + token
	base := pp + "http/t.example/dir/page"
	got := RewriteWithBase("../up", pp, base, "p.example")
	want := pp + "http/t.example/up"
	if got != want {
		t.Fatalf("dot-dot relative = %q, want %q", got, want)
	}
}

func TestProtocolRelativeSelfUserinfoStripped(t *testing.T) {
	pp := "https://p.example" + token
	got := RewriteWithBase("//u:p@p.example/x", pp, pp+"http/t.example/page", "p.example")
	if strings.Contains(got, "u:p@") || got != "//p.example/x" {
		t.Fatalf("self protocol-relative userinfo not stripped: %q", got)
	}
}

func TestUnknownCharsetKeepsOriginalBytes(t *testing.T) {
	raw := []byte{0x81, 0x82, 0xff}
	html, used := decodeHTMLBody(raw, "text/html; charset=x-unknown")
	if used != "" || html != string(raw) {
		t.Fatalf("unknown HTML charset: used=%q html=%q want raw", used, html)
	}
	txt, used := decodeTextBody(raw, "text/css; charset=x-unknown")
	if used != "" || string(txt) != string(raw) {
		t.Fatalf("unknown text charset: used=%q txt=%q want raw", used, txt)
	}
}
