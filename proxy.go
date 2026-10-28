package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// Proxy implements the core request/response pipeline.
type Proxy struct {
	cfg    *Config
	client *http.Client
}

// proxyHost is the host of the public proxy URL, used to detect and avoid
// self-referential rewrites (v2.7.4).
func (p *Proxy) proxyHost() string { return hostOfURL(p.cfg.ProxyURL) }

// NewProxy builds the reverse proxy with a tuned http.Client.
func NewProxy(cfg *Config, client *http.Client) (*Proxy, error) {
	if client == nil {
		client = newHTTPClient()
	}
	return &Proxy{cfg: cfg, client: client}, nil
}

// newHTTPClient creates the outbound HTTP client.
//
//   - DisableCompression: we manage Accept-Encoding ourselves so that we can
//     transparently decode (and rewrite) compressed text bodies.
//   - CheckRedirect is disabled: redirects must be surfaced to the browser
//     (with a rewritten Location header) instead of being followed silently,
//     otherwise cookies / scope sensitive state breaks.
func newHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 60 * time.Second}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		DisableCompression:    true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: insecureSkipVerify()},
	}
	return &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// ServeHTTP proxies the request to target.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request, target *TargetInfo) {
	if p.cfg.IsBlocked(target.Host) || p.cfg.IsBlockedURL(target.URL()) {
		http.Error(w, "siteproxy: this site is blocked", http.StatusForbidden)
		return
	}

	// Cookie header oversize guard (upstream behaviour): the limit applies
	// to the cookies actually forwarded, so the proxy's own control cookies
	// (Path=/, stripped below) never count toward it. When the remaining
	// header is still too large the upstream would reject the request
	// anyway, so answer 431 immediately and expire up to two non-control
	// cookies to shrink it (v2.7.x self-healing).
	if ck := r.Header.Get("Cookie"); ck != "" {
		if len(filterControlCookies(ck)) > maxCookieHeaderBytes {
			p.serveCookieTooLarge(w, r, ck, target)
			return
		}
	}

	// WebSocket upgrade: hijack and pipe both directions.
	if isWebSocketUpgrade(r) {
		p.proxyWebSocket(w, r, target)
		return
	}

	upReq, body, err := p.buildUpstreamRequest(r, target)
	if err != nil {
		http.Error(w, "siteproxy: "+err.Error(), http.StatusBadRequest)
		return
	}
	if rc, ok := body.(io.ReadCloser); ok && rc != nil && body != nil {
		defer rc.Close()
	}

	resp, err := p.client.Do(upReq)
	if err != nil {
		// Never echo the underlying error: it contains the upstream host/port
		// and would leak the real target to the client (upstream parity).
		log.Printf("[siteproxy] proxy fetch error for %s: %v", target.Host, err)
		http.Error(w, "Proxy fetch error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	p.writeResponse(w, r, resp, target)
}

// maxCookieHeaderBytes is the inbound Cookie header limit enforced by the
// upstream node server (0xfa0 = 4000 bytes).
const maxCookieHeaderBytes = 4000

// serveCookieTooLarge reproduces the upstream oversize-cookie mitigation:
// respond 431 and expire the first two non-control cookies. Every Domain/Path
// combination the proxy has ever used is expired (upstream `_0x503638`), so
// cookies from older versions with Domain= do not survive and re-trigger 431.
func (p *Proxy) serveCookieTooLarge(w http.ResponseWriter, r *http.Request, ck string, target *TargetInfo) {
	h := w.Header()
	expired := 0
	sitePath := "/"
	if target != nil {
		sitePath = p.cfg.BasePath() + p.cfg.TokenPrefix + target.SitePrefix() + "/"
	}
	domain := p.proxyHostOnly()
	for _, part := range strings.Split(ck, ";") {
		if expired >= 2 {
			break
		}
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name := part
		if i := strings.IndexByte(part, '='); i >= 0 {
			name = part[:i]
		}
		if name == "" || strings.HasPrefix(name, "proxy_real_") {
			continue
		}
		exp := name + "=; Expires=Thu, 01 Jan 1970 00:00:00 GMT"
		for _, path := range []string{sitePath, "/"} {
			h.Add("Set-Cookie", exp+"; Path="+path)
			if domain != "" {
				h.Add("Set-Cookie", exp+"; Domain="+domain+"; Path="+path)
				h.Add("Set-Cookie", exp+"; Domain=."+domain+"; Path="+path)
			}
		}
		expired++
	}
	http.Error(w, "Request Header Fields Too Large", http.StatusRequestHeaderFieldsTooLarge)
}

// proxyHostOnly returns the proxy host without a port (cookie Domain values
// must not contain one).
func (p *Proxy) proxyHostOnly() string {
	host := hostOfURL(p.cfg.ProxyURL)
	if strings.HasPrefix(host, "[") {
		if i := strings.IndexByte(host, ']'); i >= 0 {
			return host[:i+1]
		}
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// buildUpstreamRequest clones the inbound request for the upstream target:
// URL, host, hop-by-hop / infrastructure headers, referer/origin and the
// path-isolated cookie jar are all adjusted here.
func (p *Proxy) buildUpstreamRequest(r *http.Request, target *TargetInfo) (*http.Request, io.Reader, error) {
	targetURL := target.URL()
	if !strings.HasPrefix(targetURL, "http://") && !strings.HasPrefix(targetURL, "https://") {
		return nil, nil, fmt.Errorf("invalid target URL %q", targetURL)
	}

	var body io.Reader
	if r.Body != nil && r.ContentLength != 0 {
		body = r.Body
	}

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, body)
	if err != nil {
		return nil, nil, err
	}
	if r.ContentLength > 0 {
		upReq.ContentLength = r.ContentLength
	}

	copyUpstreamHeaders(upReq.Header, r.Header)

	// Host of the target site.
	upReq.Host = target.Host

	// NOTE: the original siteproxy does not send X-Forwarded-For to
	// upstream targets, so we do not either (parity + privacy).

	// Referer / Origin: convert proxied URLs back into real ones. When the
	// client runtime supplies the real referer, its origin is authoritative:
	// browsers send `Origin: <proxy>` on POST, which must never reach the
	// upstream as-is (v2.7.11 sets Origin from siteproxy-newreferer).
	realReferer := ""
	refererFromProxy := false
	if nr := r.Header.Get("Siteproxy-Newreferer"); nr != "" {
		realReferer = nr
		refererFromProxy = true
		upReq.Header.Del("Siteproxy-Newreferer")
	} else if ref := r.Header.Get("Referer"); ref != "" {
		if real := UnrewriteURLWithBase(ref, p.cfg.TokenPrefix, p.cfg.BasePath()); real != ref {
			realReferer = real
			refererFromProxy = true
		} else {
			realReferer = ref
		}
	}
	if realReferer != "" {
		upReq.Header.Set("Referer", realReferer)
		if refererFromProxy &&
			(strings.HasPrefix(realReferer, "http://") || strings.HasPrefix(realReferer, "https://")) {
			upReq.Header.Set("Origin", originOf(realReferer))
		}
	}
	if upReq.Header.Get("Origin") == "" {
		if org := r.Header.Get("Origin"); org != "" {
			if real := UnrewriteURLWithBase(org, p.cfg.TokenPrefix, p.cfg.BasePath()); real != org {
				upReq.Header.Set("Origin", originOf(real))
			} else if sameOrigin(org, p.cfg.ProxyURL) {
				// No usable referer: browsers still send the proxy origin.
				upReq.Header.Set("Origin", target.Protocol+"://"+target.Host)
			} else {
				upReq.Header.Set("Origin", org)
			}
		}
	}

	// We accept all encodings we are able to decode.
	upReq.Header.Set("Accept-Encoding", "gzip, deflate, br")

	// Cookies: the browser already path-filters cookies per site (each
	// target site's cookies are stored under Path={token}{proto}/{host}),
	// so the inbound Cookie header only carries cookies of the current
	// target site. Strip the proxy's own control cookies (they live at
	// Path=/ and would otherwise leak across targets), then forward.
	if ck := r.Header.Get("Cookie"); ck != "" {
		if filtered := filterControlCookies(ck); filtered != "" {
			upReq.Header.Set("Cookie", filtered)
		} else {
			upReq.Header.Del("Cookie")
		}
	} else {
		upReq.Header.Del("Cookie")
	}

	return upReq, body, nil
}

// writeResponse streams/rewrites the upstream response back to the browser.
func (p *Proxy) writeResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, target *TargetInfo) {
	outHeader := w.Header()
	needsProcess := bodyNeedsProcess(resp)
	isRedirect := resp.StatusCode >= 300 && resp.StatusCode < 400

	// Decompress on the fly before rewriting. When the encoding cannot be
	// decoded here (zstd, corrupt stream, unknown coding), the body must be
	// forwarded VERBATIM with its Content-Encoding intact - stripping the
	// header would make the browser render undecodable bytes (the upstream
	// relies on the same rule: only encodings it can decode are consumed).
	// Redirect bodies are discarded, so there is nothing to decode.
	body := resp.Body
	if needsProcess && !isRedirect {
		if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
			if dec, ok := newDecoder(enc, body); ok {
				body = io.NopCloser(dec)
			} else {
				needsProcess = false
			}
		}
	}

	rewriteResponseHeaders(p.cfg, resp, outHeader, target, needsProcess)

	// Control cookies (HttpOnly): let the server-side out-of-scope repair
	// and the service worker resolve the current site even when page
	// scripts have not set their own copies yet. The upstream appends
	// them ONLY for text bodies it actually rewrites (html/js/css with
	// status < 500) - matching that keeps Cookie headers small.
	if target != nil && needsProcess {
		outHeader.Add("Set-Cookie",
			"proxy_real_protocol="+target.Protocol+"; Path=/; HttpOnly")
		outHeader.Add("Set-Cookie",
			"proxy_real_host="+target.Host+"; Path=/; HttpOnly")
	}

	if r.Method == http.MethodHead {
		outHeader.Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		return
	}

	// Redirects never forward a body (upstream parity): only the status,
	// Location and representation headers reach the browser. Content-Type
	// stays as the origin sent it (304 is exempt from the text/plain
	// fallback applied to untyped bodies).
	if isRedirect {
		outHeader.Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		return
	}

	flusher, canFlush := w.(http.Flusher)

	// Non-processed bodies stream verbatim (binary/video/zip/SSE...).
	if !needsProcess {
		w.WriteHeader(resp.StatusCode)
		if canFlush {
			flusher.Flush()
		}
		// Flush after every chunk: SSE / streaming responses must reach the
		// browser as the upstream produces them, not after the whole body has
		// been buffered (upstream parity; the previous io.CopyBuffer only
		// flushed once at EOF).
		buf := make([]byte, 32*1024)
		var dst io.Writer = w
		if canFlush {
			dst = flushWriter{w: w, f: flusher}
		}
		_, _ = io.CopyBuffer(dst, body, buf)
		return
	}

	ct := contentType(resp.Header.Get("Content-Type"))
	var out []byte
	var truncated bool
	switch {
	case strings.HasPrefix(ct, "text/html"):
		out, truncated = p.renderHTML(resp, body, target)
		if !truncated {
			outHeader.Set("Content-Type", "text/html; charset=UTF-8")
			outHeader.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		}
	case strings.HasPrefix(ct, "text/css"):
		out, truncated = p.renderCSS(resp, body, target)
		if !truncated {
			outHeader.Set("Content-Type", "text/css; charset=UTF-8")
		}
	default: // javascript and anything else text-ish
		out, truncated = p.renderJS(resp, body, target)
		if !truncated {
			outHeader.Set("Content-Type", ct+"; charset=UTF-8")
		}
	}
	if truncated {
		// Too large to rewrite: forward the decompressed bytes unchanged
		// instead of silently truncating the page. The decoder may have more
		// data buffered, so stream the remainder after the already-read part.
		outHeader.Del("Content-Encoding")
		outHeader.Del("Content-Length")
		if strings.HasPrefix(ct, "text/html") {
			// The bytes are not transcoded, so advertise the charset the
			// browser will actually use. Upstream gives the in-document meta
			// priority over the header; keeping a wrong header (e.g. UTF-8 on
			// a GBK page) would render the whole oversized page as mojibake.
			cs, isUTF16 := htmlBodyCharset(out, resp.Header.Get("Content-Type"))
			if cs == "" || isUTF8(cs) {
				cs = "UTF-8"
			}
			outHeader.Set("Content-Type", "text/html; charset="+cs)
			// Inject the client runtime when the encoding is ASCII-compatible
			// (the tag is pure ASCII, so GBK/Big5 bytes are untouched).
			// UTF-16 bodies cannot carry an ASCII snippet.
			if !isUTF16 {
				out = injectSnippetBytes(out, injectedScriptTagAt(p.cfg.BasePath()))
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(out)
		_, _ = io.Copy(w, body)
		return
	}
	if len(out) == 0 {
		// Upstream parity: an empty body is not injected into and is
		// forwarded as-is (the original returns body=undefined).
		outHeader.Del("Content-Type")
		outHeader.Set("Content-Length", "0")
		w.WriteHeader(resp.StatusCode)
		return
	}
	outHeader.Set("Content-Length", fmt.Sprint(len(out)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}

// flushWriter flushes the underlying http.Flusher after every successful
// write, so streamed chunks are delivered immediately.
type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if n > 0 && fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

// publicPrefix is "{proxy_url}{token_prefix}", the absolute prefix every
// rewritten URL is built with (same shape as the original siteproxy).
func (p *Proxy) publicPrefix() string { return p.cfg.ProxyURL + p.cfg.TokenPrefix }

// renderHTML buffers, rewrites and injects scripts into HTML documents.
func (p *Proxy) renderHTML(resp *http.Response, body io.Reader, target *TargetInfo) ([]byte, bool) {
	raw, truncated := readAllLimited(body, maxRewriteBody)
	if truncated {
		return raw, true
	}
	if len(raw) == 0 {
		return raw, false
	}
	html, _ := decodeHTMLBody(raw, resp.Header.Get("Content-Type"))
	base := p.cfg.ProxyURL + p.cfg.TokenPrefix + target.SitePrefix() + target.Path
	inject := renderInlineBootstrap(p.cfg, target.Protocol, target.Host) + injectedScriptTagAt(p.cfg.BasePath())
	return []byte(rewriteHTML(html, p.publicPrefix(), base, p.proxyHost(), inject)), false
}

// renderCSS rewrites url(...) / @import inside stylesheets (upstream node
// pipeline).
func (p *Proxy) renderCSS(resp *http.Response, body io.Reader, target *TargetInfo) ([]byte, bool) {
	raw, truncated := readAllLimited(body, maxRewriteBody)
	if truncated {
		return raw, true
	}
	if len(raw) == 0 {
		return raw, false
	}
	txt, _ := decodeTextBody(raw, resp.Header.Get("Content-Type"))
	base := p.cfg.ProxyURL + p.cfg.TokenPrefix + target.SitePrefix() + target.Path
	return []byte(rewriteCSS(string(txt), p.publicPrefix(), base, p.proxyHost())), false
}

// renderJS rewrites the window.location assignment forms inside JavaScript
// files (upstream node shape) and prepends the ___location bootstrap line.
// Static string literals are intentionally left untouched - the injected
// client runtime traps fetch / XHR / sendBeacon at call time instead.
func (p *Proxy) renderJS(resp *http.Response, body io.Reader, target *TargetInfo) ([]byte, bool) {
	raw, truncated := readAllLimited(body, maxRewriteBody)
	if truncated {
		return raw, true
	}
	if len(raw) == 0 {
		return raw, false
	}
	txt, _ := decodeTextBody(raw, resp.Header.Get("Content-Type"))
	return []byte(jsBootstrapLine + rewriteJSFile(string(txt))), false
}

// ----------------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------------

// maxRewriteBody caps how much of a text body we buffer for rewriting
// (16 MiB). Larger bodies are streamed untouched.
const maxRewriteBody = 16 << 20

// readAllLimited reads r fully, reporting whether the limit was exceeded.
func readAllLimited(r io.Reader, limit int64) ([]byte, bool) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil && len(data) == 0 {
		return nil, false
	}
	return data, int64(len(data)) > limit
}

func contentType(ct string) string {
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

// bodyNeedsProcess decides whether the response body must be buffered and
// rewritten (text types) or streamed as-is.
// An EMPTY content-type is NOT processed: the upstream node pipeline defaults
// a missing content-type to text/plain (never rewritten), and rewriting an
// unknown binary payload as JS would corrupt it.
func bodyNeedsProcess(resp *http.Response) bool {
	if resp.StatusCode == http.StatusNotModified {
		return false // 304 carries no body; keep representation headers as-is
	}
	if resp.StatusCode >= 500 {
		return false // upstream keeps error bodies verbatim (status < 500 rewrite)
	}
	ct := contentType(resp.Header.Get("Content-Type"))
	if ct == "" {
		return false
	}
	switch {
	case strings.HasPrefix(ct, "text/html"),
		strings.HasPrefix(ct, "text/css"),
		isJavaScriptContentType(ct):
		return true
	default:
		return false
	}
}

func isJavaScriptContentType(ct string) bool {
	switch ct {
	case "application/javascript", "text/javascript",
		"application/x-javascript", "text/x-javascript",
		"application/ecmascript", "text/ecmascript",
		"application/x-ecmascript", "text/x-ecmascript":
		return true
	}
	return strings.HasSuffix(ct, "+js")
}

// sameOrigin compares two absolute URLs by scheme://host.
func sameOrigin(a, b string) bool {
	return strings.EqualFold(originOf(a), originOf(b))
}

// originOf returns scheme://host for an absolute URL.
func originOf(u string) string {
	i := strings.Index(u, "://")
	if i < 0 {
		return u
	}
	rest := u[i+3:]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		return u[:i+3] + rest[:j]
	}
	return u
}

func insecureSkipVerify() bool {
	v := insecureSkipVerifyEnv()
	return v == "1" || strings.EqualFold(v, "true")
}
