package main

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSSEFirstChunkArrivesBeforeUpstreamCompletes guards the streaming path:
// the proxy must flush each chunk as it is read instead of buffering the whole
// body until EOF.
func TestSSEFirstChunkArrivesBeforeUpstreamCompletes(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		fmt.Fprint(w, "data: first\n\n")
		f.Flush()
		time.Sleep(400 * time.Millisecond)
		fmt.Fprint(w, "data: second\n\n")
		f.Flush()
	}))
	defer up.Close()

	ts, base := startProxy(t, up.URL)
	start := time.Now()
	resp, err := http.Get(ts.URL + base + "/sse")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	firstAt := time.Since(start)
	if err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	if line != "data: first\n" {
		t.Fatalf("first line = %q", line)
	}
	if firstAt > 250*time.Millisecond {
		t.Errorf("first SSE chunk was buffered: arrived after %v (want < 250ms)", firstAt)
	}
}

// TestSubpathInjectionAndServiceWorker verifies that a proxy mounted under a
// base path emits base-path-aware runtime/worker URLs and scope.
func TestSubpathInjectionAndServiceWorker(t *testing.T) {
	up := startMockUpstream(t)
	cfg := newTestConfig(0)
	cfg.ProxyURL = "https://p.example/sub"
	cfg.TokenPrefix = "/tok/"
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	host := strings.TrimPrefix(up.URL, "http://")
	_, body := proxyGet(ts, "/sub/tok/http/"+host+"/html", nil)
	wantTag := `<script src="/sub/siteproxy-response-injected.js?v=4"></script>`
	if !strings.Contains(string(body), wantTag) {
		t.Errorf("injected runtime tag missing base path %q:\n%s", wantTag, body)
	}

	resp, js := proxyGet(ts, "/sub/siteproxy-response-injected.js", nil)
	if resp == nil || resp.StatusCode != 200 {
		t.Fatalf("injected script status = %v", resp)
	}
	if !strings.Contains(string(js), "(proxyBasePath || '') + '/siteproxy_service_worker.js") {
		t.Errorf("service worker URL is not base-path aware")
	}
	if !strings.Contains(string(js), "var swScope = (proxyBasePath || '') + '/'") ||
		!strings.Contains(string(js), "scope: swScope") {
		t.Errorf("service worker scope is not base-path aware")
	}

	resp, _ = proxyGet(ts, "/sub/siteproxy_service_worker.js?proxy_real_protocol=https&proxy_real_host=t.example", nil)
	if resp == nil || resp.StatusCode != 200 {
		t.Fatalf("service worker status = %v", resp)
	}
	if got := resp.Header.Get("Service-Worker-Allowed"); got != "/sub/" {
		t.Errorf("Service-Worker-Allowed = %q, want %q", got, "/sub/")
	}
}

// TestSetCookieWhitespaceAttributes ensures "Path = /" / "Domain = ..." (with
// spaces around '=') are parsed and isolated instead of bypassing rewriting.
func TestSetCookieWhitespaceAttributes(t *testing.T) {
	cfg := &Config{ProxyURL: "https://p.example", TokenPrefix: "/tok/"}
	got := rewriteSetCookie("sid=1; Path = /deep; Domain = .evil.example; Secure", cfg.TokenPrefix, "https/t.example", cfg)
	if strings.Contains(strings.ToLower(got), "domain") {
		t.Errorf("spaced Domain attribute not dropped: %q", got)
	}
	if !strings.Contains(got, "Path=/tok/https/t.example/deep") {
		t.Errorf("spaced Path attribute not isolated: %q", got)
	}

	got = rewriteSetCookie("x=1; path = foo", cfg.TokenPrefix, "https/t.example", cfg)
	if !strings.Contains(got, "Path=/tok/https/t.example/foo") {
		t.Errorf("relative Path value not rooted: %q", got)
	}
}

// TestProxyFetchErrorHidesUpstreamDetails ensures dial errors are not echoed to
// the client (they contain the real upstream host/port).
func TestProxyFetchErrorHidesUpstreamDetails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	cfg := newTestConfig(0)
	cfg.ProxyURL = "http://127.0.0.1"
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, body := proxyGet(ts, token+"http/"+addr+"/x", nil)
	if resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %v", resp)
	}
	if strings.Contains(string(body), addr) || strings.Contains(strings.ToLower(string(body)), "dial") {
		t.Errorf("upstream details leaked: %q", body)
	}
	if strings.TrimSpace(string(body)) != "Proxy fetch error" {
		t.Errorf("body = %q, want %q", body, "Proxy fetch error")
	}
}
