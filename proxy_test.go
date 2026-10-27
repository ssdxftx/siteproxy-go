package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// ----------------------------------------------------------------------------
// Test infrastructure: a mock upstream that can emit HTML/CSS/JS, set
// cookies, redirect, compress, transcode charsets, stream SSE and echo
// WebSocket frames.
// ----------------------------------------------------------------------------

func newTestConfig(port int) *Config {
	return &Config{
		ProxyURL:        fmt.Sprintf("http://127.0.0.1:%d", port),
		TokenPrefix:     "/user22334455/",
		LocalListenPort: port,
	}
}

func startMockUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		fmt.Fprint(w, `<!DOCTYPE html><html><head><title>t</title>
<link rel="stylesheet" href="/style.css">
<script src="/app.js" integrity="sha384-abc" crossorigin="anonymous"></script>
</head><body>
<a href="https://other.example.com/path?x=1">abs</a>
<a href="//cdn.example.com/lib.js">proto-rel</a>
<a href="/root.html">root</a>
<a href="rel.html">rel</a>
<img src="/logo.png" srcset="/a.png 1x, /b.png 2x">
<iframe src="/frame"></iframe>
<div style="background:url('/bg.png')">styled</div>
<style>.i{background:url(/inner.png)}</style>
<form action="/submit" method="post"><input name="q"></form>
<meta http-equiv="refresh" content="3; url=/next.html">
</body></html>`)
	})

	mux.HandleFunc("/style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		fmt.Fprint(w, `@import url("/fonts.css"); body{background:url("https://img.example.com/x.png")}`)
	})

	mux.HandleFunc("/app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		fmt.Fprint(w, `var api="https://api.example.com/v1";var proto="//cdn.example.com/a.js";
var comment="see https://docs.example.com/help";var x="//";var re=/https?:\/\/[^/]+/g;
window.location.href = "/dashboard";
if (window.location == a) { }`)
	})

	mux.HandleFunc("/cookie", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "sid=abc123; Domain=.example.com; Path=/; Secure; HttpOnly; SameSite=None")
		w.Header().Add("Set-Cookie", "plain=1; Path=/deep/path")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"cookies":true}`)
	})

	mux.HandleFunc("/redir", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://www.example.com/target?a=1", http.StatusFound)
	})

	mux.HandleFunc("/redir-rel", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/relative", http.StatusFound)
	})

	mux.HandleFunc("/gzhtml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		fmt.Fprint(gz, `<html><body><a href="https://gz.example.com/x">gz</a></body></html>`)
		gz.Close()
	})

	mux.HandleFunc("/brhtml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "br")
		bw := brotli.NewWriter(w)
		fmt.Fprint(bw, `<html><body><a href="https://br.example.com/x">br</a></body></html>`)
		bw.Close()
	})

	mux.HandleFunc("/gbk", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=gbk")
		enc := simplifiedchinese.GBK.NewEncoder()
		out, err := enc.Bytes([]byte(`<html><head><title>编码测试</title></head><body><h1>中文内容测试</h1></body></html>`))
		if err != nil {
			t.Errorf("gbk encode: %v", err)
		}
		w.Write(out)
	})

	mux.HandleFunc("/meta-gbk", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html") // no charset in header
		enc := simplifiedchinese.GBK.NewEncoder()
		out, _ := enc.Bytes([]byte(`<html><head><meta charset="gbk"><title>元标记</title></head><body>内容</body></html>`))
		w.Write(out)
	})

	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: tick %d\n\n", i)
			flusher.Flush()
			time.Sleep(50 * time.Millisecond)
		}
	})

	mux.HandleFunc("/unquoted", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><a href=/uq1>l1</a><img src=/uq2.png><img src="/uq3.png"></body></html>`)
	})

	mux.HandleFunc("/binary", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte{0, 1, 2, 3, 255, 254})
	})

	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		// Echo request characteristics back as JSON for assertions.
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"host":%q,"referer":%q,"origin":%q,"xff":%q,"cookie":%q,
"ua":%q,"method":%q,"requrl":%q}`,
			r.Host, r.Header.Get("Referer"), r.Header.Get("Origin"),
			r.Header.Get("X-Forwarded-For"), r.Header.Get("Cookie"),
			r.Header.Get("User-Agent"), r.Method, r.URL.RequestURI())
	})

	mux.HandleFunc("/echo-headers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var b strings.Builder
		b.WriteString("{")
		first := true
		for k, v := range r.Header {
			if !first {
				b.WriteString(",")
			}
			first = false
			fmt.Fprintf(&b, "%q:%q", strings.ToLower(k), strings.Join(v, "|"))
		}
		b.WriteString("}")
		fmt.Fprint(w, b.String())
	})

	mux.HandleFunc("/resp-headers", func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/plain")
		h.Set("Cf-Ray", "abc123")
		h.Set("X-Forwarded-Foo", "edge")
		h.Set("Siteproxy-Internal", "secret")
		h.Set("Via", "1.1 edge-node")
		fmt.Fprint(w, "ok")
	})

	return httptest.NewServer(mux)
}

// startProxy builds a full siteproxy Server fronting the mock upstream and
// returns (server, proxiedBase) where proxiedBase is
// "{token}http/{mock-host}" — the path prefix that reaches the mock.
func startProxy(t *testing.T, upstreamURL string) (*httptest.Server, string) {
	t.Helper()
	cfg := newTestConfig(0)
	cfg.ProxyURL = upstreamURL
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	host := upstreamURL[strings.Index(upstreamURL, "://")+3:]
	return ts, token + "http/" + host
}

func proxyGet(ts *httptest.Server, path string, hdr map[string]string) (*http.Response, []byte) {
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, body
}

const token = "/user22334455/"

// ----------------------------------------------------------------------------
// URL mapping unit tests
// ----------------------------------------------------------------------------

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in                string
		proto, host, path string
		ok                bool
	}{
		{"/https/example.com/", "https", "example.com", "/", true},
		{"/https/example.com", "https", "example.com", "/", true},
		{"/http/example.com/a/b", "http", "example.com", "/a/b", true},
		{"/https/sub.example.com:8080/x", "https", "sub.example.com:8080", "/x", true},
		{"/ftp/example.com/", "", "", "", false},
		{"/https/nodot/", "https", "nodot", "/", true},
		{"/https/localhost/", "https", "localhost", "/", true},
		{"/https/@evil.com/", "", "", "", false},
		{"/index.html", "", "", "", false},
	}
	for _, c := range cases {
		got := ParseTarget(c.in, "")
		if c.ok && got == nil {
			t.Errorf("ParseTarget(%q) = nil, want ok", c.in)
			continue
		}
		if !c.ok {
			if got != nil {
				t.Errorf("ParseTarget(%q) = %v, want nil", c.in, got)
			}
			continue
		}
		if got.Protocol != c.proto || got.Host != c.host || got.Path != c.path {
			t.Errorf("ParseTarget(%q) = %s/%s/%s, want %s/%s/%s",
				c.in, got.Protocol, got.Host, got.Path, c.proto, c.host, c.path)
		}
	}
}

func TestRewriteURL(t *testing.T) {
	pub := "http://p.com/user1/"
	if got := RewriteURL("https://x.com/a?b=1", pub, "p.com"); got != pub+"https/x.com/a?b=1" {
		t.Errorf("RewriteURL abs = %q", got)
	}
	if got := RewriteURL("http://p.com/self", pub, "p.com"); got != "http://p.com/self" {
		t.Errorf("RewriteURL self-loop = %q", got)
	}
	if got := RewriteURL(pub+"https/already.com/x", pub, "p.com"); got != pub+"https/already.com/x" {
		t.Errorf("RewriteURL double-wrap = %q", got)
	}
	base := "http://p.com/user1/https/h.com/dir/page"
	if got := RewriteWithBase("/root", pub, base, "p.com"); got != pub+"https/h.com/root" {
		t.Errorf("RewriteWithBase root = %q", got)
	}
	if got := RewriteWithBase("img.png", pub, base, "p.com"); got != pub+"https/h.com/dir/img.png" {
		t.Errorf("RewriteWithBase rel = %q", got)
	}
	if got := RewriteWithBase("//cdn.x.com/a.js", pub, "http://p.com/user1/https/h.com/", "p.com"); got != pub+"https/cdn.x.com/a.js" {
		t.Errorf("RewriteWithBase proto-rel = %q", got)
	}
	if got := RewriteWithBase("data:image/png;base64,AAA", pub, "http://p.com/", "p.com"); got != "data:image/png;base64,AAA" {
		t.Errorf("data URL must not be rewritten: %q", got)
	}
}

func TestFilterControlCookies(t *testing.T) {
	in := "proxy_real_host=h.com; proxy_real_protocol=https; sid=xyz; a=1"
	want := "sid=xyz; a=1"
	if got := filterControlCookies(in); got != want {
		t.Errorf("filterControlCookies = %q, want %q", got, want)
	}
	if got := filterControlCookies("proxy_real_host=h.com"); got != "" {
		t.Errorf("control-only cookie = %q, want empty", got)
	}
	if got := filterControlCookies("ok=1"); got != "ok=1" {
		t.Errorf("normal cookie mangled: %q", got)
	}
}

// ----------------------------------------------------------------------------
// Integration tests
// ----------------------------------------------------------------------------

func TestHTMLRewriteAndInjection(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)

	resp, body := proxyGet(ts, base+"/html", nil)
	if resp == nil {
		t.Fatal("no response")
	}
	html := string(body)

	// Injection.
	if !strings.Contains(html, `<meta http-equiv="Content-Type" content="text/html; charset=UTF-8">`) {
		t.Error("charset meta not injected")
	}
	if !strings.Contains(html, `<script src="/siteproxy-response-injected.js?v=4"></script>`) {
		t.Error("runtime script not injected")
	}

	// Security headers stripped.
	if resp.Header.Get("Content-Security-Policy") != "" {
		t.Error("CSP not stripped")
	}
	if resp.Header.Get("X-Frame-Options") != "" {
		t.Error("X-Frame-Options not stripped")
	}

	// Attribute rewrites.
	if !strings.Contains(html, token+"https/other.example.com/path?x=1") {
		t.Errorf("absolute href not rewritten")
	}
	if !strings.Contains(html, token+"https/cdn.example.com/lib.js") {
		t.Errorf("protocol-relative href not rewritten")
	}
	if !strings.Contains(html, base+"/root.html") {
		t.Errorf("root-relative href not rewritten:\n%s", html)
	}
	if !strings.Contains(html, base+"/rel.html") {
		t.Errorf("relative href not rewritten")
	}
	if !strings.Contains(html, base+"/logo.png") {
		t.Errorf("img src not rewritten")
	}
	if !strings.Contains(html, base+"/a.png 1x") || !strings.Contains(html, base+"/b.png 2x") {
		t.Errorf("srcset candidates not rewritten")
	}
	if !strings.Contains(html, base+"/frame") {
		t.Errorf("iframe src not rewritten")
	}
	if !strings.Contains(html, base+"/bg.png") {
		t.Errorf("style attr url() not rewritten: %s", html)
	}
	if !strings.Contains(html, `url(`+up.URL+base+`/inner.png`) {
		t.Errorf("style tag url() not rewritten")
	}
	if !strings.Contains(html, base+"/submit") {
		t.Errorf("form action not rewritten")
	}
	if !strings.Contains(html, base+"/next.html") {
		t.Errorf("meta refresh not rewritten")
	}
	// integrity/crossorigin dropped.
	if strings.Contains(html, `integrity=`) || strings.Contains(html, `crossorigin=`) {
		t.Errorf("integrity/crossorigin not dropped:\n%s", html)
	}
	// CSS link rewritten.
	if !strings.Contains(html, base+"/style.css") {
		t.Errorf("link href not rewritten")
	}
	if !strings.Contains(html, base+"/app.js") {
		t.Errorf("script src not rewritten")
	}
}

func TestCSSRewrite(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)
	_, body := proxyGet(ts, base+"/style.css", nil)
	css := string(body)
	if !strings.Contains(css, base+"/fonts.css") {
		t.Errorf("@import not rewritten: %s", css)
	}
	if !strings.Contains(css, token+"https/img.example.com/x.png") {
		t.Errorf("absolute css url not rewritten: %s", css)
	}
}

func TestJSRewrite(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)
	_, body := proxyGet(ts, base+"/app.js", nil)
	js := string(body)
	// Like the original, string literals are NOT rewritten server-side;
	// the injected runtime traps fetch/XHR/sendBeacon at call time.
	if !strings.Contains(js, `var api="https://api.example.com/v1"`) {
		t.Errorf("js absolute literal must stay untouched: %s", js)
	}
	if !strings.Contains(js, `var proto="//cdn.example.com/a.js"`) {
		t.Errorf("js proto-rel literal must stay untouched")
	}
	// The regex literal must stay untouched.
	if !strings.Contains(js, `/https?:\/\/[^/]+/g`) {
		t.Errorf("regex literal corrupted: %s", js)
	}
	if !strings.Contains(js, `var x="//"`) {
		t.Errorf("short // literal corrupted")
	}
	// window.location assignment routed through wrapper.
	if !strings.Contains(js, "window.___location.href") {
		t.Errorf("window.location.href not wrapped: %s", js)
	}
	// The original prepends a ___location bootstrap to every JS body.
	if !strings.HasPrefix(js, "if(typeof self!=='undefined'&&typeof self.___location==='undefined')") {
		t.Errorf("___location bootstrap line missing: %s", js)
	}
}

func TestSetCookieIsolation(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)
	resp, _ := proxyGet(ts, base+"/cookie", nil)
	if resp == nil {
		t.Fatal("no response")
	}
	cookies := resp.Header.Values("Set-Cookie")
	// The /cookie endpoint has no Content-Type, so the proxy does NOT
	// append its control cookies here (upstream appends them only for
	// rewritten text bodies): exactly the 2 isolated upstream cookies.
	if len(cookies) != 2 {
		t.Fatalf("want 2 Set-Cookie, got %d: %v", len(cookies), cookies)
	}
	c1 := cookies[0] // sid
	if strings.Contains(c1, "Domain=") {
		t.Errorf("Domain not dropped: %s", c1)
	}
	if !strings.Contains(c1, "Path="+base+"/") {
		t.Errorf("Path not isolated: %s", c1)
	}
	if !strings.Contains(c1, "HttpOnly") {
		t.Errorf("HttpOnly lost: %s", c1)
	}
	if strings.Contains(c1, "SameSite=None") {
		t.Errorf("SameSite not normalised to Lax: %s", c1)
	}
	if !strings.Contains(c1, "SameSite=Lax") {
		t.Errorf("SameSite=Lax missing: %s", c1)
	}
	// Secure kept when proxy is http? our test proxy is http → dropped.
	if strings.Contains(c1, "Secure") {
		t.Errorf("Secure must be dropped on plain-HTTP proxy: %s", c1)
	}
	c2 := cookies[1] // plain
	if !strings.Contains(c2, "Path="+base+"/deep/path") {
		t.Errorf("orig Path not preserved: %s", c2)
	}
	// Control cookies: appended only for text-rewritten (html) responses
	// - fetch one and verify shape.
	resp2, _ := proxyGet(ts, base+"/html", nil)
	var ctrlProto, ctrlHost string
	for _, c := range resp2.Header.Values("Set-Cookie") {
		if strings.HasPrefix(c, "proxy_real_protocol=") {
			ctrlProto = c
		}
		if strings.HasPrefix(c, "proxy_real_host=") {
			ctrlHost = c
		}
	}
	if ctrlProto == "" || ctrlHost == "" {
		t.Errorf("control cookies missing on html response: %v", resp2.Header.Values("Set-Cookie"))
	} else {
		if !strings.Contains(ctrlProto, "Path=/; HttpOnly") ||
			!strings.Contains(ctrlHost, "Path=/; HttpOnly") {
			t.Errorf("control cookies must be HttpOnly at Path=/: %v | %v", ctrlProto, ctrlHost)
		}
	}
}

func TestRedirectRewrite(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	get := func(path string) *http.Response {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	resp := get(base + "/redir")
	if resp.StatusCode != 302 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	want := up.URL + token + "https/www.example.com/target?a=1"
	if loc := resp.Header.Get("Location"); loc != want {
		t.Errorf("Location = %q, want %q", loc, want)
	}

	resp = get(base + "/redir-rel")
	if loc := resp.Header.Get("Location"); loc != up.URL+base+"/relative" {
		t.Errorf("relative Location = %q", loc)
	}
}

func TestGzipAndBrotliDecode(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)

	_, body := proxyGet(ts, base+"/gzhtml", nil)
	if !strings.Contains(string(body), token+"https/gz.example.com/x") {
		t.Errorf("gzip html not decoded/rewritten: %s", body)
	}
	_, body = proxyGet(ts, base+"/brhtml", nil)
	if !strings.Contains(string(body), token+"https/br.example.com/x") {
		t.Errorf("brotli html not decoded/rewritten: %s", body)
	}
}

func TestGbkTranscode(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)

	_, body := proxyGet(ts, base+"/gbk", nil)
	if !strings.Contains(string(body), "中文内容测试") {
		t.Errorf("GBK header-charset not transcoded: %q", body)
	}
	if !strings.Contains(string(body), "charset=UTF-8") {
		t.Errorf("UTF-8 meta not injected")
	}

	_, body = proxyGet(ts, base+"/meta-gbk", nil)
	if !strings.Contains(string(body), "内容") {
		t.Errorf("GBK meta-charset not transcoded: %q", body)
	}
}

func TestBinaryPassThrough(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)
	_, body := proxyGet(ts, base+"/binary", nil)
	if !bytes.Equal(body, []byte{0, 1, 2, 3, 255, 254}) {
		t.Errorf("binary body corrupted: %v", body)
	}
}

func TestSSEStreaming(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)

	start := time.Now()
	req, _ := http.NewRequest("GET", ts.URL+base+"/sse", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}
	br := bufio.NewReader(resp.Body)
	n := 0
	for n < 3 {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended early: %v", err)
		}
		if strings.HasPrefix(line, "data: tick") {
			n++
		}
	}
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		t.Errorf("SSE was buffered, elapsed=%v (want >= 3 flush intervals)", elapsed)
	}
}

func TestRequestHeaderTransformation(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)

	// A browser-ish request: referer points at another proxied page.
	ref := ts.URL + token + "https/from.example.com/page"
	resp, body := proxyGet(ts, base+"/echo",
		map[string]string{
			"Referer":                   ref,
			"Origin":                    ts.URL,
			"Cf-Connecting-Ip":          "1.2.3.4",
			"X-Forwarded-For":           "1.2.3.4",
			"Cookie":                    "sid=xyz",
			"Siteproxy-Newreferer":      "https://target.example.com/real",
			"Upgrade-Insecure-Requests": "1",
		})
	if resp == nil {
		t.Fatal("no response")
	}
	s := string(body)
	wantHost := strings.TrimPrefix(up.URL, "http://")
	if !strings.Contains(s, `"host":"`+wantHost+`"`) {
		t.Errorf("Host not rewritten: %s", s)
	}
	if !strings.Contains(s, `"referer":"https://target.example.com/real"`) {
		t.Errorf("siteproxy-newreferer not honoured: %s", s)
	}
}

func TestControlCookiesNotForwarded(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)
	_, body := proxyGet(ts, base+"/echo",
		map[string]string{
			"Cookie": "proxy_real_host=127.0.0.1:19001; proxy_real_protocol=http; sid=keep1; plain=2",
		})
	if body == nil {
		t.Fatal("no response")
	}
	s := string(body)
	if !strings.Contains(s, `"cookie":"sid=keep1; plain=2"`) {
		t.Errorf("control cookies not filtered (or order changed): %s", s)
	}
	if strings.Contains(s, "proxy_real") {
		t.Errorf("control cookie leaked upstream: %s", s)
	}
}

func TestInfraHeadersStripped(t *testing.T) {
	up := startMockUpstream(t)
	ts, _ := startProxy(t, up.URL)
	resp, body := proxyGet(ts, token+"https://x/echo-headers",
		map[string]string{
			"Cf-Ray":            "abc",
			"Cdn-Loop":          "cloudflare",
			"X-Forwarded-Proto": "https",
			"Via":               "1.1 somewhere",
		})
	_ = resp
	s := strings.ToLower(string(body))
	for _, banned := range []string{"cloudflare", "1.1 somewhere"} {
		if strings.Contains(s, banned) {
			t.Errorf("infra header leaked upstream: %s", s)
		}
	}
}

func TestBlockedSite(t *testing.T) {
	up := startMockUpstream(t)
	cfg := newTestConfig(0)
	cfg.ProxyURL = up.URL
	cfg.BlockedSites = []string{"bad.example.com"}
	srv, _ := NewServer(cfg)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, _ := proxyGet(ts, token+"https/bad.example.com/x", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("blocked site status = %d, want 403", resp.StatusCode)
	}
	sub, _ := proxyGet(ts, token+"https/sub.bad.example.com/x", nil)
	if sub.StatusCode != http.StatusForbidden {
		t.Errorf("blocked subdomain status = %d, want 403", sub.StatusCode)
	}
}

func TestTokenGate(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)
	_ = base

	// Wrong token → homepage.
	resp, body := proxyGet(ts, "/wrongtoken/https/evil.com/", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "<title>Siteproxy</title>") {
		t.Errorf("wrong token did not fall back to homepage")
	}

	// Correct token → proxied.
	resp, _ = proxyGet(ts, base+"/binary", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("proxied status = %d", resp.StatusCode)
	}
}

func TestNoPasswordMode(t *testing.T) {
	up := startMockUpstream(t)
	cfg := newTestConfig(0)
	cfg.ProxyURL = up.URL
	cfg.TokenPrefix = "/"
	srv, _ := NewServer(cfg)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, body := proxyGet(ts, "/http/"+strings.TrimPrefix(up.URL, "http://")+"/binary", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !bytes.Equal(body, []byte{0, 1, 2, 3, 255, 254}) {
		t.Errorf("body = %v", body)
	}
}

func TestHeadRequest(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)
	req, _ := http.NewRequest("HEAD", ts.URL+base+"/html", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("HEAD status = %d", resp.StatusCode)
	}
}

func TestConfigEnvOverrides(t *testing.T) {
	f := "siteproxy_test_" + fmt.Sprint(time.Now().UnixNano()) + ".json"
	os.WriteFile(f, []byte(`{"proxy_url":"https://cfg.example.com","token_prefix":"/tok/","local_listen_port":1234}`), 0o600)
	defer os.Remove(f)

	t.Setenv("SITEPROXY_PROXY_URL", "https://env.example.com")
	t.Setenv("SITEPROXY_TOKEN_PREFIX", "/envtok/")
	t.Setenv("SITEPROXY_LOCAL_LISTEN_PORT", "7777")
	t.Setenv("SITEPROXY_BLOCKED_SITES", "a.com, b.com")

	cfg, err := LoadConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProxyURL != "https://env.example.com" || cfg.TokenPrefix != "/envtok/" ||
		cfg.LocalListenPort != 7777 {
		t.Errorf("env overrides failed: %+v", cfg)
	}
	joined := strings.Join(cfg.BlockedSites, ",")
	if !strings.Contains(joined, "a.com") || !strings.Contains(joined, "b.com") {
		t.Errorf("blocked sites env override failed: %v", cfg.BlockedSites)
	}
}

func TestWebSocketProxy(t *testing.T) {
	// Echo websocket server (raw HTTP hijack, no external deps).
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWebSocketUpgrade(r) {
			http.Error(w, "not ws", 400)
			return
		}
		hj := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		defer conn.Close()
		key := r.Header.Get("Sec-WebSocket-Key")
		accept := wsAccept(key)
		fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
		// Minimal RFC6455 echo: unmask the client frame, echo it unmasked.
		hdr := make([]byte, 2)
		for {
			if _, err := io.ReadFull(conn, hdr); err != nil {
				return
			}
			length := int(hdr[1] & 0x7f)
			mask := make([]byte, 4)
			if _, err := io.ReadFull(conn, mask); err != nil {
				return
			}
			payload := make([]byte, length)
			if _, err := io.ReadFull(conn, payload); err != nil {
				return
			}
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
			out := append([]byte{0x81, byte(length)}, payload...)
			if _, err := conn.Write(out); err != nil {
				return
			}
		}
	}))
	defer up.Close()

	ts, base := startProxy(t, up.URL)

	// Handshake through the proxy.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + base + "/echo"
	hdr := http.Header{}
	hdr.Set("Upgrade", "websocket")
	hdr.Set("Connection", "Upgrade")
	hdr.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	hdr.Set("Sec-WebSocket-Version", "13")
	req := &http.Request{Method: "GET", URL: mustParse(wsURL), Header: hdr}
	conn, err := netDialTCP(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("ws handshake status = %d", resp.StatusCode)
	}

	// Send a masked text frame ("hello", mask key 1,2,3,4) expect echo.
	payload := []byte("hello")
	mask := []byte{1, 2, 3, 4}
	frame := []byte{0x81, 0x80 | byte(len(payload))}
	frame = append(frame, mask...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	if _, err := conn.Write(frame); err != nil {
		t.Fatal(err)
	}
	echoHdr := make([]byte, 2)
	if _, err := io.ReadFull(br, echoHdr); err != nil {
		t.Fatalf("echo read: %v", err)
	}
	if echoHdr[0] != 0x81 || echoHdr[1] != 0x05 {
		t.Fatalf("unexpected echo frame header: %v", echoHdr)
	}
	echoPayload := make([]byte, 5)
	if _, err := io.ReadFull(br, echoPayload); err != nil {
		t.Fatalf("echo payload read: %v", err)
	}
	if string(echoPayload) != "hello" {
		t.Errorf("echo payload = %q", echoPayload)
	}
}

// wsAccept computes Sec-WebSocket-Accept (RFC 6455) without external deps.
func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + magicGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

const magicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func TestUnquotedAttributes(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)
	_, body := proxyGet(ts, base+"/unquoted", nil)
	if body == nil {
		t.Fatal("no response")
	}
	html := string(body)
	for _, want := range []string{
		`href=` + up.URL + base + `/uq1>`,
		`src=` + up.URL + base + `/uq2.png>`,
		`src="` + up.URL + base + `/uq3.png"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("unquoted attr rewrite failed, want %q in:\n%s", want, html)
		}
	}
}
