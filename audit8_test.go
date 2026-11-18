package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// P-41: Max-Age wins over Expires when both are present. The rewrite must
// emit exactly one Max-Age instead of preserving/capping each attribute
// independently.
func TestSetCookieMultipleLifetimeAttrsHasSingleMaxAge(t *testing.T) {
	cfg := &Config{ProxyURL: "https://p.example", TokenPrefix: "/t/", CookieMaxAge: 1800}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "max-age before far expires",
			in:   "a=1; Path=/; Max-Age=60; Expires=Wed, 21 Oct 2030 07:28:00 GMT",
			want: "Max-Age=60",
		},
		{
			name: "far expires before max-age",
			in:   "b=1; Path=/; Expires=Wed, 21 Oct 2030 07:28:00 GMT; Max-Age=60",
			want: "Max-Age=60",
		},
		{
			name: "long max-age and far expires",
			in:   "c=1; Path=/; Max-Age=99999; Expires=Wed, 21 Oct 2030 07:28:00 GMT",
			want: "Max-Age=1800",
		},
		{
			name: "past expires wins over long max-age",
			in:   "d=1; Path=/; Max-Age=99999; Expires=Thu, 01 Jan 1970 00:00:00 GMT",
			want: "Expires=Thu, 01 Jan 1970 00:00:00 GMT",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteSetCookie(tc.in, "/t/", "https/t.example", cfg)
			if strings.Count(got, "Max-Age=") > 1 {
				t.Fatalf("duplicate Max-Age: %s", got)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("rewriteSetCookie(%q) = %q, want substring %q", tc.in, got, tc.want)
			}
		})
	}
}

// P-42: Telegram CSS must keep the original charset bytes. Decoding to UTF-8
// without rewriting @charset makes the browser decode the new bytes using
// the old declaration.
func TestTelegramLegacyCSSKeepsOriginalBytesAndHeader(t *testing.T) {
	raw, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(`@charset "gbk";a{content:"中文"}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{ProxyURL: "http://proxy.test", TokenPrefix: token}
	p := &Proxy{cfg: cfg}
	target := &TargetInfo{Protocol: "https", Host: "web.telegram.org", Path: "/style.css"}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/css; charset=gbk"}},
		Body:       io.NopCloser(bytes.NewReader(raw)),
	}
	out, truncated, err := p.renderCSS(resp, resp.Body, target)
	if err != nil || truncated {
		t.Fatalf("renderCSS: err=%v truncated=%v", err, truncated)
	}
	if !bytes.Equal(out, raw) {
		t.Errorf("Telegram CSS bytes changed:\n got %x\nwant %x", out, raw)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	resp = &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/css; charset=gbk"}},
		Body:       io.NopCloser(bytes.NewReader(raw)),
	}
	p.writeResponse(rec, req, resp, target)
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(strings.ToLower(ct), "charset=gbk") {
		t.Errorf("Content-Type = %q, want GBK", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), raw) {
		t.Errorf("response body bytes changed:\n got %x\nwant %x", rec.Body.Bytes(), raw)
	}
}

// P-43 and P-45 are client-runtime fixes; keep lightweight source guards so
// the two branches cannot quietly disappear again.
func TestClientUnrewriteRootAndTraverseRoot(t *testing.T) {
	js := string(assetsInjected)
	if !strings.Contains(js, "if (!rest || rest.charAt(0) === '?') real += '/'") {
		t.Error("unRewriteURL no longer normalises the target root slash")
	}
	if !strings.Contains(js, "if (root && root.nodeType === 1) rewriteElement(root);") {
		t.Error("traverse no longer processes the supplied root element")
	}
}

// P-44: only background-style requests delay a 404; POST/PUT/PATCH must
// return immediately, matching the upstream service worker.
func TestServiceWorker404DelayExcludesWriteMethods(t *testing.T) {
	js := string(assetsWorker)
	for _, want := range []string{
		"const isWriteMethod = method === 'POST' || method === 'PUT' || method === 'PATCH';",
		"resp.status === 404 && !isWriteMethod",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("service worker missing %q", want)
		}
	}
}
