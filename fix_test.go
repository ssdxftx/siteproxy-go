package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCSSDataURINotRewritten(t *testing.T) {
	pp := "http://p.example" + token
	base := pp + "http/t.example/dir/page"
	cases := []string{
		`.x{background:url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg'%3E%3C/svg%3E")}`,
		`.x{background:url(data:image/svg+xml;utf8,<svg xmlns="http://www.w3.org/2000/svg"></svg>)}`,
	}
	for _, in := range cases {
		if got := rewriteCSS(in, pp, base, "p.example"); got != in {
			t.Errorf("data URI was rewritten:\n in: %s\nout: %s", in, got)
		}
	}
}

func TestMetaRefreshQuotedURL(t *testing.T) {
	pp := "http://p.example" + token
	base := pp + "http/t.example/dir/page"
	in := `<meta http-equiv="refresh" content="0; url='https://x/y'">`
	want := `<meta http-equiv="refresh" content="0; url='` + pp + `https/x/y'">`
	if got := rewriteMetaRefresh(in, pp, base, "p.example"); got != want {
		t.Errorf("quoted meta refresh:\n got %q\nwant %q", got, want)
	}
	in = `<meta http-equiv="refresh" content='0; url="https://x/y"'>`
	want = `<meta http-equiv="refresh" content='0; url="` + pp + `https/x/y"'>`
	if got := rewriteMetaRefresh(in, pp, base, "p.example"); got != want {
		t.Errorf("double-quoted meta refresh:\n got %q\nwant %q", got, want)
	}
}

func TestEmptyHTMLBodyKeepsContentType(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	ts, base := startProxy(t, up.URL)
	resp, body := proxyGet(ts, base+"/empty", nil)
	if resp == nil {
		t.Fatal("no response")
	}
	if len(body) != 0 {
		t.Errorf("empty body = %q", body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("empty HTML lost Content-Type: %q", ct)
	}
}

func TestHeadDoesNotSetControlCookies(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)

	req, _ := http.NewRequest(http.MethodHead, ts.URL+base+"/html", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, c := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(c, "proxy_real_") {
			t.Errorf("HEAD must not set control cookie: %s", c)
		}
	}
	// Upstream parity: HEAD keeps the origin size rather than dropping it.
	if cl := resp.Header.Get("Content-Length"); cl == "" {
		t.Error("HEAD lost the upstream Content-Length")
	}
}

func TestHeadKeepsContentEncodingAndLength(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	ts, base := startProxy(t, up.URL)

	req, _ := http.NewRequest(http.MethodHead, ts.URL+base+"/compressed", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("HEAD Content-Encoding = %q, want gzip", got)
	}
	if got := resp.Header.Get("Content-Length"); got != "10" {
		t.Errorf("HEAD Content-Length = %q, want 10", got)
	}
}

func TestTruncatedUpstreamReturns502(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		defer conn.Close()
		fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nContent-Length: 100\r\n\r\n<html>")
	}))
	defer up.Close()

	ts, base := startProxy(t, up.URL)
	resp, body := proxyGet(ts, base+"/truncated", nil)
	if resp == nil {
		t.Fatal("no response")
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("truncated upstream status = %d, want 502 (body %q)", resp.StatusCode, body)
	}
}

func TestDecodeBytesUnknownCharsetKeepsBytes(t *testing.T) {
	raw := []byte{0xff, 0xfe, 0x41}
	s, used := decodeBytes(raw, "x-foo")
	if used != "" || s != string(raw) {
		t.Errorf("decodeBytes changed unknown-charset bytes: %q (used %q)", s, used)
	}
}

func TestSameSitePreservedOnHTTPS(t *testing.T) {
	cfg := &Config{ProxyURL: "https://p.example", TokenPrefix: "/t/"}
	got := rewriteSetCookie("x=1; Path=/; SameSite=None", "/t/", "https/t.example", cfg)
	if !strings.Contains(got, "SameSite=None") {
		t.Errorf("HTTPS must preserve SameSite=None: %s", got)
	}
	got = rewriteSetCookie("x=1; Path=/; SameSite=Strict", "/t/", "https/t.example", cfg)
	if !strings.Contains(got, "SameSite=Strict") {
		t.Errorf("HTTPS must preserve SameSite=Strict: %s", got)
	}

	cfg = &Config{ProxyURL: "http://p.example", TokenPrefix: "/t/"}
	got = rewriteSetCookie("x=1; Path=/; SameSite=None", "/t/", "https/t.example", cfg)
	if !strings.Contains(got, "SameSite=Lax") {
		t.Errorf("HTTP must normalise SameSite=None to Lax: %s", got)
	}
}

func TestBasePathRejectsOutsidePath(t *testing.T) {
	up := startMockUpstream(t)
	cfg := newTestConfig(0)
	cfg.ProxyURL = "https://p.example/sub"
	cfg.TokenPrefix = "/t/"
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	host := strings.TrimPrefix(up.URL, "http://")
	resp, body := proxyGet(ts, "/t/http/"+host+"/html", nil)
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("outside-base-path request must not proxy: status=%v body=%q", resp, body)
	}

	resp, body = proxyGet(ts, "/sub/t/http/"+host+"/html", nil)
	if resp == nil || resp.StatusCode != 200 || strings.Contains(string(body), "<title>Siteproxy</title>") {
		t.Errorf("inside-base-path request must proxy: status=%v body=%q", resp, body)
	}
}

func TestLinkHeaderRewritten(t *testing.T) {
	pp := "http://p.example" + token
	got := rewriteLinkHeader(`<https://x/a>; rel=preload, </b>; rel=preload`, pp, "http/t.example", "p.example")
	want := `<` + pp + `https/x/a>; rel=preload, <` + pp + `http/t.example/b>; rel=preload`
	if got != want {
		t.Errorf("Link header:\n got %q\nwant %q", got, want)
	}
}

func TestSetCookieMaxAgeCap(t *testing.T) {
	cfg := &Config{ProxyURL: "https://p.example", TokenPrefix: "/t/", CookieMaxAge: 1800}
	got := rewriteSetCookie("a=1; Path=/; Expires=Wed, 21 Oct 2030 07:28:00 GMT", "/t/", "https/t.example", cfg)
	if !strings.Contains(got, "Max-Age=1800") || strings.Contains(got, "Expires=") {
		t.Errorf("future expiry should be capped: %s", got)
	}
	got = rewriteSetCookie("a=1; Path=/; Expires=Thu, 01 Jan 1970 00:00:00 GMT", "/t/", "https/t.example", cfg)
	if strings.Contains(got, "Max-Age=1800") || !strings.Contains(got, "Expires=Thu") {
		t.Errorf("past expiry must stay a deletion: %s", got)
	}
	got = rewriteSetCookie("a=1; Path=/; Max-Age=0", "/t/", "https/t.example", cfg)
	if strings.Contains(got, "Max-Age=1800") || !strings.Contains(got, "Max-Age=0") {
		t.Errorf("Max-Age=0 must stay a deletion: %s", got)
	}
	got = rewriteSetCookie("proxy_real_host=x; Path=/; Expires=Wed, 21 Oct 2030 07:28:00 GMT", "/t/", "https/t.example", cfg)
	if strings.Contains(got, "Max-Age=") {
		t.Errorf("control cookie must not be capped: %s", got)
	}
	cfg.CookieMaxAge = 0
	got = rewriteSetCookie("a=1; Path=/; Expires=Wed, 21 Oct 2030 07:28:00 GMT", "/t/", "https/t.example", cfg)
	if !strings.Contains(got, "Expires=") {
		t.Errorf("cap disabled must preserve expiry: %s", got)
	}
}
