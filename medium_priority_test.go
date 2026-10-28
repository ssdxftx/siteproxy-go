package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNELHeaderStripped(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.Header().Set("NEL", `{"report_to":"default","max_age":86400}`)
		w.Header().Set("Report-To", `{"group":"default"}`)
		_, _ = io.WriteString(w, "<html><head></head><body>ok</body></html>")
	}))
	defer up.Close()

	ts, base := startProxy(t, up.URL)
	resp, _ := proxyGet(ts, base+"/nel", nil)
	if resp == nil {
		t.Fatal("nil response")
	}
	if got := resp.Header.Get("Nel"); got != "" {
		t.Fatalf("NEL header leaked to client: %q", got)
	}
	if got := resp.Header.Get("Report-To"); got != "" {
		t.Fatalf("Report-To header leaked to client: %q", got)
	}
}

func TestBuiltinBlocklistMatchesUpstream(t *testing.T) {
	cfg, err := (&Config{
		ProxyURL:        "http://localhost:5006",
		TokenPrefix:     "/t/",
		LocalListenPort: 5006,
	}).normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}

	// Upstream v2.7.11 blocks exactly these three entries.
	for _, host := range []string{"www.visa.com.sg", "netsuite.com", "visa.com"} {
		if cfg.IsBlocked(host) {
			t.Errorf("%s must not be blocked (not in upstream default list)", host)
		}
	}
	if !cfg.IsBlocked("www.elcomercio.pe") {
		t.Error("elcomercio.pe should be blocked")
	}
	if !cfg.IsBlocked("exchangebank.com") {
		t.Error("exchangebank.com should be blocked")
	}
	if !cfg.IsBlockedURL("https://telegram.org/service_worker.js") {
		t.Error("telegram.org/service_worker.js should be blocked")
	}
}

func TestDocumentDomainHasNoopSetter(t *testing.T) {
	src := string(assetsInjected)
	idx := strings.Index(src, "Object.defineProperty(document, 'domain'")
	if idx < 0 {
		t.Fatal("document.domain override missing")
	}
	block := src[idx:]
	if end := strings.Index(block, "});"); end >= 0 {
		block = block[:end]
	}
	if !strings.Contains(block, "set: function ()") {
		t.Fatalf("document.domain override has no no-op setter:\n%s", block)
	}
}

func TestOversizedGBKHTMLKeepsCharsetAndInjectsRuntime(t *testing.T) {
	head := []byte(`<html><head><meta charset="gbk"><title>big</title></head><body>`)
	gbk := []byte{0xc4, 0xe3, 0xba, 0xc3} // "你好" in GBK
	tail := append([]byte("<p>"), gbk...)
	tail = append(tail, []byte("</p></body></html>")...)
	pad := bytes.Repeat([]byte("A"), int(maxRewriteBody)+(1<<20))
	original := make([]byte, 0, len(head)+len(pad)+len(tail))
	original = append(original, head...)
	original = append(original, pad...)
	original = append(original, tail...)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately wrong header: the in-document meta says gbk, so the
		// bytes must be advertised as gbk, not UTF-8.
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		_, _ = w.Write(original)
	}))
	defer up.Close()

	ts, base := startProxy(t, up.URL)
	resp, body := proxyGet(ts, base+"/big-gbk", nil)
	if resp == nil {
		t.Fatal("nil response")
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(strings.ToLower(ct), "charset=gbk") {
		t.Fatalf("Content-Type = %q, want charset=gbk", ct)
	}

	tag := injectedScriptTag
	idx := bytes.Index(body, []byte(tag))
	if idx < 0 {
		t.Fatal("runtime script tag not injected into oversized HTML")
	}
	withoutTag := append(append([]byte{}, body[:idx]...), body[idx+len(tag):]...)
	if !bytes.Equal(withoutTag, original) {
		t.Fatalf("oversized body corrupted: got %d bytes, want %d", len(withoutTag), len(original))
	}
	if !bytes.HasSuffix(body, tail) {
		t.Fatal("GBK tail bytes were altered")
	}
}
