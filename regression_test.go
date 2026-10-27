package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

func TestRewriteWithBaseKeepsNonHTTPSchemes(t *testing.T) {
	pp := "https://p.example" + token
	base := pp + "https/t.example/a/b"
	cases := []string{
		"ftp://files.example/x",
		"sms:12345",
		"file:///etc/hosts",
		"chrome-extension://abc/page.html",
		"vbscript:msgbox(1)",
	}
	for _, in := range cases {
		if got := RewriteWithBase(in, pp, base, "p.example"); got != in {
			t.Errorf("RewriteWithBase(%q) = %q, want unchanged", in, got)
		}
	}
	if got := RewriteWithBase(token+"https/x.example/y", pp, base, "p.example"); got != token+"https/x.example/y" {
		t.Errorf("token-prefixed URL double-wrapped: %q", got)
	}
}

func TestRewriteWithBaseQueryOnlyRelative(t *testing.T) {
	pp := "https://p.example" + token
	base := pp + "https/t.example/a/b"
	got := RewriteWithBase("?page=2", pp, base, "p.example")
	want := base + "?page=2"
	if got != want {
		t.Errorf("query-only relative = %q, want %q", got, want)
	}
	if got := RewriteWithBase("c.html", pp, base, "p.example"); got != pp+"https/t.example/a/c.html" {
		t.Errorf("relative path = %q", got)
	}
}

func TestOriginHeaderRestoredFromNewReferer(t *testing.T) {
	up := startMockUpstream(t)
	ts, base := startProxy(t, up.URL)
	_, body := proxyGet(ts, base+"/echo", map[string]string{
		"Origin":               ts.URL,
		"Siteproxy-Newreferer": "https://target.example.com/real",
	})
	if !strings.Contains(string(body), `"origin":"https://target.example.com"`) {
		t.Errorf("Origin not restored from newreferer: %s", body)
	}

	// Proxied Referer without siteproxy-newreferer must also restore Origin.
	ref := ts.URL + token + "https/from.example.com/page"
	_, body = proxyGet(ts, base+"/echo", map[string]string{
		"Origin":  ts.URL,
		"Referer": ref,
	})
	if !strings.Contains(string(body), `"origin":"https://from.example.com"`) {
		t.Errorf("Origin not restored from Referer: %s", body)
	}
}

func TestCSSAndJSCharsetHeaderUpdated(t *testing.T) {
	gbk := func(s string) []byte {
		out, _ := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(s))
		return out
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/style.css":
			w.Header().Set("Content-Type", "text/css; charset=gbk")
			_, _ = w.Write(gbk(`body{color:red}/* 中文 */`))
		case "/app.js":
			w.Header().Set("Content-Type", "application/javascript; charset=gbk")
			_, _ = w.Write(gbk(`var s="中文";`))
		}
	}))
	defer up.Close()
	cfg := newTestConfig(0)
	cfg.ProxyURL = up.URL
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	host := strings.TrimPrefix(up.URL, "http://")
	for _, path := range []string{"/style.css", "/app.js"} {
		resp, body := proxyGet(ts, token+"http/"+host+path, nil)
		if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "charset=utf-8") {
			t.Errorf("%s Content-Type = %q, want UTF-8", path, resp.Header.Get("Content-Type"))
		}
		if !strings.Contains(string(body), "中文") {
			t.Errorf("%s body not transcoded: %q", path, body)
		}
	}
}

func Test5xxBodyIsNotRewritten(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `<html><head></head><body>boom</body></html>`)
	}))
	defer up.Close()
	cfg := newTestConfig(0)
	cfg.ProxyURL = up.URL
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	host := strings.TrimPrefix(up.URL, "http://")
	resp, body := proxyGet(ts, token+"http/"+host+"/err", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if strings.Contains(string(body), "siteproxy-response-injected.js") {
		t.Errorf("500 body was rewritten:\n%s", body)
	}
	for _, c := range resp.Header.Values("Set-Cookie") {
		if strings.Contains(c, "proxy_real_") {
			t.Errorf("control cookie set on 500: %s", c)
		}
	}
}

func TestEmptyTokenPrefixUsesUpstreamDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"proxy_url":"https://p.example","token_prefix":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TokenPrefix != defaultTokenPrefix {
		t.Errorf("empty token_prefix = %q, want %q", cfg.TokenPrefix, defaultTokenPrefix)
	}
	if got := normalizeTokenPrefix("/"); got != "/" {
		t.Errorf("explicit / = %q, want /", got)
	}
}

func TestServiceWorkerContextFromQuery(t *testing.T) {
	cfg := newTestConfig(0)
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, body := proxyGet(ts, "/siteproxy_service_worker.js?proxy_real_protocol=https&proxy_real_host=t.example", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), `proxy_real_host: "t.example"`) ||
		!strings.Contains(string(body), `proxy_real_protocol: "https"`) {
		t.Errorf("service worker context missing:\n%s", string(body))
	}
}

func TestHomepageLinksAreAbsolute(t *testing.T) {
	cfg := newTestConfig(0)
	cfg.ProxyURL = "https://p.example"
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, body := proxyGet(ts, "/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	want := "https://p.example" + token + "https/github.com/netptop/siteproxy"
	if !strings.Contains(string(body), want) {
		t.Errorf("homepage link not absolute:\nmissing %q", want)
	}
}

func TestJSIdentifierScanner(t *testing.T) {
	in := "var s = \"location\";\n" +
		"var r = /location/g;\n" +
		"// location comment\n" +
		"var t = `x ${location.href}`;\n" +
		"var my_location = 1;\n" +
		"location.href = '/next';\n"
	got := rewriteJSIdentifiers(in)
	if !strings.Contains(got, `var s = "location";`) {
		t.Errorf("string literal was changed:\n%s", got)
	}
	if !strings.Contains(got, `var r = /location/g;`) {
		t.Errorf("regex literal was changed:\n%s", got)
	}
	if !strings.Contains(got, "// location comment") {
		t.Errorf("comment was changed:\n%s", got)
	}
	if !strings.Contains(got, "`x ${___location.href}`") {
		t.Errorf("template expression not rewritten:\n%s", got)
	}
	if !strings.Contains(got, "var my_location = 1;") {
		t.Errorf("longer identifier was changed:\n%s", got)
	}
	if !strings.Contains(got, "___location.href = '/next';") {
		t.Errorf("bare location not rewritten:\n%s", got)
	}
}

func TestScriptBodiesAreNotURLRewritten(t *testing.T) {
	pp := "https://p.example" + token
	base := pp + "https/t.example/a/b"
	html := `<html><head><script>` +
		`var tpl = '<a href="https://x.example/a">';` +
		`var r = /href="y"/;` +
		`location.href = "/z";` +
		`var s = ' integrity="sha384-keep"';` +
		`</script><script src="/app.js" integrity="sha384-x" crossorigin="anonymous"></script></head></html>`
	got := rewriteHTML(html, pp, base, "p.example",
		renderInlineBootstrap(newTestConfig(0), "https", "t.example")+injectedScriptTag)
	if !strings.Contains(got, `var tpl = '<a href="https://x.example/a">';`) {
		t.Errorf("inline script URL string was rewritten:\n%s", got)
	}
	if !strings.Contains(got, `var r = /href="y"/;`) {
		t.Errorf("inline script regex was rewritten:\n%s", got)
	}
	if !strings.Contains(got, `var s = ' integrity="sha384-keep"';`) {
		t.Errorf("inline script integrity string was stripped:\n%s", got)
	}
	if !strings.Contains(got, `___location.href = "/z";`) {
		t.Errorf("inline location not rewritten:\n%s", got)
	}
	if strings.Contains(got, `integrity="sha384-x"`) || strings.Contains(got, `crossorigin="anonymous"`) {
		t.Errorf("script tag integrity/crossorigin not removed:\n%s", got)
	}
}

func TestHTMLAttributeNamesCaseInsensitive(t *testing.T) {
	pp := "https://p.example" + token
	base := pp + "https/t.example/dir/page.html"
	html := `<A HREF="/x">x</A>` +
		`<IMG SRCSET="/a.png 1x, /b.png 2x">` +
		`<DIV STYLE="background:url(/bg.png)"></DIV>` +
		`<BASE HREF="/dir/">`
	got := rewriteHTML(html, pp, base, "p.example", "")
	for _, want := range []string{
		pp + "https/t.example/x",
		pp + "https/t.example/a.png 1x",
		pp + "https/t.example/b.png 2x",
		pp + "https/t.example/bg.png",
		pp + "https/t.example/dir/",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("uppercase attribute not rewritten, missing %q:\n%s", want, got)
		}
	}
}

func TestCSSURLWithEntityQuotes(t *testing.T) {
	pp := "https://p.example" + token
	base := pp + "https/t.example/dir/page.html"
	cases := []struct{ in, want string }{
		{`a{background:url(&quot;/x&quot;)}`, `a{background:url(&quot;` + pp + `https/t.example/x&quot;)}`},
		{`a{background:url(&#34;/y&#34;)}`, `a{background:url(&#34;` + pp + `https/t.example/y&#34;)}`},
		{`a{background:url('&quot;/x&quot;')}`, `a{background:url('&quot;/x&quot;')}`},
		{`a{background:url("/z")}`, `a{background:url("` + pp + `https/t.example/z")}`},
	}
	for _, c := range cases {
		if got := rewriteCSS(c.in, pp, base, "p.example"); got != c.want {
			t.Errorf("rewriteCSS(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestFragmentAndTextHrefNotRewritten(t *testing.T) {
	pp := "https://p.example" + token
	base := pp + "https/t.example/dir/page.html"
	if got := RewriteWithBase("#top", pp, base, "p.example"); got != "#top" {
		t.Errorf("fragment rewritten: %q", got)
	}
	html := `<p>href="/x"</p><a href="#top">x</a>`
	got := rewriteHTML(html, pp, base, "p.example", "")
	if !strings.Contains(got, `href="/x"`) {
		t.Errorf("text href was rewritten:\n%s", got)
	}
	if !strings.Contains(got, `href="#top"`) {
		t.Errorf("fragment href was rewritten:\n%s", got)
	}
}

func TestInlineJSAndQuotedAttributesRewritten(t *testing.T) {
	pp := "https://p.example" + token
	base := pp + "https/t.example/dir/page.html"
	html := `<button onclick="location.href='/x'">x</button>` +
		`<a href="javascript:location.href='/y'">y</a>` +
		`<div data-foo="https://x.com/a"></div>` +
		`<object data="/o.svg"></object>` +
		`<button onclick="alert(&quot;location&quot;)">q</button>` +
		`<div once="location">o</div>`
	got := rewriteHTML(html, pp, base, "p.example", "")
	for _, want := range []string{
		`onclick="___location.href='` + pp + `https/t.example/x'"`,
		`javascript:___location.href='` + pp + `https/t.example/y'`,
		`data-foo="` + pp + `https/x.com/a"`,
		`data="` + pp + `https/t.example/o.svg"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !strings.Contains(got, `alert(&quot;location&quot;)`) {
		t.Errorf("entity-quoted handler string was corrupted:\n%s", got)
	}
	if !strings.Contains(got, `once="location"`) {
		t.Errorf("non-event on* attribute was rewritten:\n%s", got)
	}
}

func TestCSSImageSetQuotedURLRewritten(t *testing.T) {
	pp := "https://p.example" + token
	base := pp + "https/t.example/dir/page.html"
	in := `.a{background:image-set("https://x.com/a.png" 1x, "/b.png" 2x)}`
	want := `.a{background:image-set("` + pp + `https/x.com/a.png" 1x, "` + pp + `https/t.example/b.png" 2x)}`
	if got := rewriteCSS(in, pp, base, "p.example"); got != want {
		t.Errorf("rewriteCSS(%q)\n got %q\nwant %q", in, got, want)
	}
}

func TestSetCookieCombinedAndPreisolated(t *testing.T) {
	cfg := &Config{ProxyURL: "https://p.example", TokenPrefix: "/tok/"}
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", "a=1; Path=/, b=2; Path=/; HttpOnly")
	resp.Header.Add("Set-Cookie", "c=3; Path=/tok/https/t.example/already")
	resp.Header.Add("Set-Cookie", "d=4; Expires=Thu, 01 Jan 1970 00:00:00 GMT")
	resp.Header.Add("Set-Cookie", "broken")
	out := http.Header{}
	rewriteResponseHeaders(cfg, resp, out, &TargetInfo{Protocol: "https", Host: "t.example"}, false)
	cookies := out.Values("Set-Cookie")
	if len(cookies) != 4 {
		t.Fatalf("want 4 cookies, got %d: %v", len(cookies), cookies)
	}
	joined := strings.Join(cookies, "\n")
	for _, want := range []string{
		"a=1; Path=/tok/https/t.example/",
		"b=2; Path=/tok/https/t.example/; HttpOnly",
		"c=3; Path=/tok/https/t.example/already",
		"d=4; Path=/tok/https/t.example/; Expires=Thu, 01 Jan 1970",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, "/tok/https/t.example/tok/https") {
		t.Errorf("cookie path was double-prefixed:\n%s", joined)
	}
}

func TestBlockedDomainWithPort(t *testing.T) {
	cfg := &Config{BlockedSites: []string{"elcomercio.pe"}}
	for _, host := range []string{"elcomercio.pe:443", "sub.elcomercio.pe:443", "ELCOMERCIO.PE.", "elcomercio.pe.:443"} {
		if !cfg.IsBlocked(host) {
			t.Errorf("IsBlocked(%q) = false", host)
		}
	}
	if cfg.IsBlocked("elcomercio.pe.evil:443") {
		t.Error("suffix match too broad")
	}
}

func TestIsPlausibleHostRejectsInjection(t *testing.T) {
	valid := []string{"t.example", "localhost", "127.0.0.1:8080", "[::1]:8080", "sub.example:443"}
	for _, h := range valid {
		if !isPlausibleHost(h) {
			t.Errorf("isPlausibleHost(%q) = false", h)
		}
	}
	invalid := []string{"", "x@evil.com", "evil.com/x", "evil.com?x", "evil.com#x",
		"evil.com\\x", "evil.com:abc", "evil.com:70000", "evil.com:0", "[::1", "exa mple.com"}
	for _, h := range invalid {
		if isPlausibleHost(h) {
			t.Errorf("isPlausibleHost(%q) = true", h)
		}
	}
}

func TestUnrewriteURLKeepsEscapedPath(t *testing.T) {
	got := UnrewriteURL("https://p.example/tok/https/t.example/a%2Fb?q=1", "/tok/")
	want := "https://t.example/a%2Fb?q=1"
	if got != want {
		t.Errorf("UnrewriteURL escaped path:\n got %q\nwant %q", got, want)
	}
}

func TestSetCookiePathIncludesProxyBasePath(t *testing.T) {
	cfg := &Config{ProxyURL: "https://p.example/sub", TokenPrefix: "/tok/"}
	got := rewriteSetCookie("a=1; Path=/", cfg.TokenPrefix, "https/t.example", cfg)
	if !strings.Contains(got, "Path=/sub/tok/https/t.example/") {
		t.Errorf("cookie path must include proxy base path: %q", got)
	}
}

func TestSubpathDeploymentRoutingAndUnrewrite(t *testing.T) {
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
	resp, body := proxyGet(ts, "/sub/tok/http/"+host+"/html", nil)
	if resp == nil || resp.StatusCode != 200 {
		t.Fatalf("subpath proxy status = %v", resp)
	}
	if !strings.Contains(string(body), "https://p.example/sub/tok/https/other.example.com/path") {
		t.Errorf("subpath rewrite missing base path:\n%s", body)
	}

	got := UnrewriteURLWithBase("https://p.example/sub/tok/https/t.example/a%2Fb?q=1", "/tok/", "/sub")
	want := "https://t.example/a%2Fb?q=1"
	if got != want {
		t.Errorf("UnrewriteURLWithBase:\n got %q\nwant %q", got, want)
	}
	if target := targetFromProxiedURL("https://p.example/sub/tok/https/t.example/dir/page", "/tok/", "/sub"); target == nil || target.Host != "t.example" || target.Path != "/dir/page" {
		t.Errorf("targetFromProxiedURL = %+v", target)
	}

	resp, body = proxyGet(ts, "/sub/", nil)
	if resp == nil || resp.StatusCode != 200 || !strings.Contains(string(body), "<title>Siteproxy</title>") {
		t.Errorf("subpath homepage failed: status=%v body=%q", resp, body)
	}
	resp, body = proxyGet(ts, "/sub/siteproxy-response-injected.js", nil)
	if resp == nil || resp.StatusCode != 200 || !strings.Contains(string(body), "siteproxy-response-injected.js") {
		t.Errorf("subpath injected script failed: status=%v", resp)
	}
}

func TestHTMLMetaCharsetWinsOverHeader(t *testing.T) {
	gbk := func(s string) []byte {
		out, _ := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(s))
		return out
	}
	raw := gbk(`<html><head><meta charset="gbk"><title>元标记</title></head><body>内容</body></html>`)
	got, cs := decodeHTMLBody(raw, "text/html; charset=iso-8859-1")
	if !strings.Contains(got, "元标记") || !strings.Contains(got, "内容") {
		t.Errorf("meta charset not preferred: %q (used %q)", got, cs)
	}
}

func TestJSObjectEscapesQuotesOnce(t *testing.T) {
	cfg := &Config{ProxyURL: "https://p.example", TokenPrefix: `/a'b/`}
	got := jsObject(cfg, "https", "t.example")
	if !strings.Contains(got, `token_prefix: "/a'b/"`) {
		t.Errorf("single quote was double-escaped: %s", got)
	}
	if strings.Contains(got, `\\'`) {
		t.Errorf("backslash-quote sequence in JS object: %s", got)
	}
}

func TestTemplateExpressionDivisionAndRegex(t *testing.T) {
	in := "const a = `${x / y} / ${location}`; const b = `${/}/.test(location)}`;"
	got := rewriteJSIdentifiers(in)
	if !strings.Contains(got, "${location}") && !strings.Contains(got, "${___location}") {
		t.Errorf("second template expression lost: %s", got)
	}
	if strings.Count(got, "___location") != 2 {
		t.Errorf("want 2 location rewrites, got: %s", got)
	}
}

func TestInvalidProxyURLRejected(t *testing.T) {
	for _, u := range []string{"ftp://p.example", "https://p.example?x=1", "https://p.example#f", "p.example", "https://user@host"} {
		cfg := &Config{ProxyURL: u, TokenPrefix: "/tok/"}
		if _, err := cfg.normalize(); err == nil {
			t.Errorf("proxy_url %q was accepted", u)
		}
	}
	for _, tok := range []string{"/to k/", "/tok?x/", "/tok#x/"} {
		cfg := &Config{ProxyURL: "https://p.example", TokenPrefix: tok}
		if _, err := cfg.normalize(); err == nil {
			t.Errorf("token_prefix %q was accepted", tok)
		}
	}
}

func TestRewriteWithBaseProtocolRelativeProxyHost(t *testing.T) {
	pub := "https://p.example/tok/"
	base := pub + "https/t.example/dir/page"
	in := "//p.example/siteproxy-response-injected.js"
	if got := RewriteWithBase(in, pub, base, "p.example"); got != in {
		t.Errorf("protocol-relative proxy URL was wrapped: %q", got)
	}
}

func TestRewriteURLUppercaseAlreadyProxied(t *testing.T) {
	pub := "https://p.example/tok/"
	in := "HTTPS://P.EXAMPLE/tok/https/x.example/a"
	if got := RewriteURL(in, pub, "p.example"); got != in {
		t.Errorf("uppercase proxied URL double-wrapped: %q", got)
	}
}

func TestRewriteWithBaseWhitespaceValueUnchanged(t *testing.T) {
	pub := "https://p.example/tok/"
	base := pub + "https/t.example/dir/page"
	for _, in := range []string{"", " ", "\t", "a b", "/x y", " /x", "/x "} {
		if got := RewriteWithBase(in, pub, base, "p.example"); got != in {
			t.Errorf("RewriteWithBase(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestHTMLCharsetDeclarationUpdatedAfterTranscode(t *testing.T) {
	gbk := func(s string) []byte {
		out, _ := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(s))
		return out
	}
	for _, body := range []string{
		`<meta charset="gbk"><div>中文</div>`,
		`<meta name="x" content="text/html; charset=gbk"><div>中文</div>`,
		`<meta http-equiv="Content-Type" content="text/html; charset=gbk"><div>中文</div>`,
		`<meta data-x="1" http-equiv="Content-Type" content="text/html; charset=gbk"><div>中文</div>`,
	} {
		raw := gbk(body)
		html, used := decodeHTMLBody(raw, "")
		if used == "" {
			t.Fatal("expected GBK transcode")
		}
		html = rewriteCharsetMeta(html)
		if strings.Contains(strings.ToLower(html), "gbk") {
			t.Errorf("legacy charset left in document: %s", html)
		}
		if !strings.Contains(html, `charset=UTF-8`) || !strings.Contains(html, "中文") {
			t.Errorf("charset meta not updated: %s", html)
		}
	}
}
