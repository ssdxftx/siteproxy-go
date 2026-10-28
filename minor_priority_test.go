package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func proxyGetNoRedirect(t *testing.T, ts *httptest.Server, path string) (*http.Response, []byte) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

func TestRedirectDropsBodyAndKeepsHTMLContentType(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Location", "/next")
		w.WriteHeader(http.StatusFound)
		fmt.Fprint(w, `<html><body><a href="/x">x</a></body></html>`)
	}))
	defer up.Close()

	ts, base := startProxy(t, up.URL)
	resp, body := proxyGetNoRedirect(t, ts, base+"/redirect")
	if resp == nil {
		t.Fatal("nil response")
	}
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html" {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	if len(body) != 0 {
		t.Fatalf("redirect body forwarded: %q", body)
	}
	if !strings.HasPrefix(resp.Header.Get("Location"), up.URL) {
		t.Fatalf("Location not proxied: %q", resp.Header.Get("Location"))
	}
	// Upstream adds the control cookies for processable redirect responses.
	var sawHost bool
	for _, c := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(c, "proxy_real_host=") {
			sawHost = true
		}
	}
	if !sawHost {
		t.Fatal("proxy_real_host control cookie missing on text/html redirect")
	}
}

func TestRedirectPlainContentTypeUnchangedAndNoControlCookies(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Location", "/next")
		w.WriteHeader(http.StatusFound)
		fmt.Fprint(w, "plain redirect")
	}))
	defer up.Close()

	ts, base := startProxy(t, up.URL)
	resp, body := proxyGetNoRedirect(t, ts, base+"/redirect")
	if resp == nil {
		t.Fatal("nil response")
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
	if len(body) != 0 {
		t.Fatalf("redirect body forwarded: %q", body)
	}
	if len(resp.Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("control cookies added to non-processable redirect: %v", resp.Header.Values("Set-Cookie"))
	}
}

func TestNotModifiedKeepsRepresentationHeaders(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("Cache-Control", "max-age=60")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer up.Close()

	ts, base := startProxy(t, up.URL)
	resp, body := proxyGet(ts, base+"/304", nil)
	if resp == nil {
		t.Fatal("nil response")
	}
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Fatalf("304 body not empty: %q", body)
	}
	if got := resp.Header.Get("ETag"); got != `"abc"` {
		t.Fatalf("ETag = %q, want preserved", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "max-age=60" {
		t.Fatalf("Cache-Control = %q, want preserved", got)
	}
	if len(resp.Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("control cookies added to 304: %v", resp.Header.Values("Set-Cookie"))
	}
}

func TestOversizedCookieCheckIgnoresControlCookies(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer up.Close()

	ts, base := startProxy(t, up.URL)

	// Control cookies alone exceed the limit; they are stripped before
	// forwarding, so the request must go through.
	resp, _ := proxyGet(ts, base+"/x", map[string]string{
		"Cookie": "proxy_real_host=" + strings.Repeat("c", 4100),
	})
	if resp == nil {
		t.Fatal("nil response")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("control-only oversized cookie: status = %d, want 200", resp.StatusCode)
	}

	// A real cookie beyond the limit still triggers the 431 mitigation.
	resp, _ = proxyGet(ts, base+"/x", map[string]string{
		"Cookie": "a=" + strings.Repeat("r", 4100),
	})
	if resp == nil {
		t.Fatal("nil response")
	}
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("oversized real cookie: status = %d, want 431", resp.StatusCode)
	}
}

func TestRenderInlineBootstrapSinglePass(t *testing.T) {
	cfg := &Config{ProxyURL: "http://__HOST__.example", TokenPrefix: "/t/"}
	got := renderInlineBootstrap(cfg, "https", "real.example")
	if !strings.Contains(got, "http://__HOST__.example") {
		t.Fatal("placeholder inside a substituted value was replaced again")
	}
	if !strings.Contains(got, `real.example`) {
		t.Fatal("__HOST__ placeholder was not substituted")
	}
}
