package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
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
		Proxy:                 nil, // opt-in via SITEPROXY_USE_ENV_PROXY
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
	if useEnvProxy() {
		transport.Proxy = http.ProxyFromEnvironment
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

	// Referer / Origin: convert proxied URLs back into real ones.
	//
	// v2.7.11 semantics (verified by differential probing):
	//   - Siteproxy-Newreferer wins; Origin is that URL's origin.
	//   - Otherwise a proxied Referer is un-rewritten, and Origin is the
	//     TARGET origin (not the referer's origin). Browsers send
	//     `Origin: <proxy>` on POST, which must never reach the upstream.
	//   - A real Referer is forwarded unchanged; a proxy Origin is replaced
	//     by the target origin, any other Origin is left alone.
	newReferer := r.Header.Get("Siteproxy-Newreferer")
	realReferer := ""
	refererFromProxy := false
	switch {
	case newReferer != "":
		realReferer = newReferer
		refererFromProxy = true
		upReq.Header.Del("Siteproxy-Newreferer")
	case r.Header.Get("Referer") != "":
		ref := r.Header.Get("Referer")
		if real, ok := p.unrewriteReferer(ref); ok {
			realReferer = real
			refererFromProxy = true
		} else {
			realReferer = ref
		}
	}
	if realReferer != "" {
		upReq.Header.Set("Referer", realReferer)
	}
	if refererFromProxy {
		if newReferer != "" {
			upReq.Header.Set("Origin", originOf(newReferer))
		} else {
			upReq.Header.Set("Origin", target.Protocol+"://"+target.Host)
		}
	}
	if upReq.Header.Get("Origin") == "" {
		if org := r.Header.Get("Origin"); org != "" {
			if real := UnrewriteURLWithBase(org, p.cfg.TokenPrefix, p.cfg.BasePath()); real != org {
				upReq.Header.Set("Origin", originOf(real))
			} else if p.isProxyOrigin(org, r) {
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

// isRedirectStatus reports whether a status code is one the upstream treats
// as a redirect: 301/302/303/307/308. Other 3xx responses (300 Multiple
// Choices, 304, 305, 306, 309) keep their body and Location header.
func isRedirectStatus(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// unrewriteReferer converts a proxied absolute Referer back to the real URL.
// Unlike UnrewriteURLWithBase it mirrors the upstream string splice exactly:
// no trailing slash is added for a bare host, malformed escapes survive, and
// the token root maps to an empty referer (which the caller still treats as
// "came from the proxy").
func (p *Proxy) unrewriteReferer(ref string) (string, bool) {
	// Work on the path, not on cfg.ProxyURL: the public URL may be a
	// different host/port than the listening socket (tests, reverse proxies),
	// and a deployment may live under a base path.
	i := strings.Index(ref, "://")
	if i < 0 {
		return "", false
	}
	rest := ref[i+3:]
	j := strings.IndexByte(rest, '/')
	if j < 0 {
		return "", false
	}
	path := rest[j:]
	query := ""
	if k := strings.IndexByte(path, '?'); k >= 0 {
		query = path[k+1:]
		path = path[:k]
	}
	if bp := p.cfg.BasePath(); bp != "" && (path == bp || strings.HasPrefix(path, bp+"/")) {
		path = strings.TrimPrefix(path, bp)
		if path == "" {
			path = "/"
		}
	}
	if tp := p.cfg.TokenPrefix; tp != "/" {
		if !strings.HasPrefix(path, tp) {
			return "", false
		}
		path = path[len(tp):]
	}
	if path == "" {
		return "", true
	}
	m := pathPrefixRe.FindStringSubmatch(path)
	if m == nil {
		return path, true
	}
	real := m[1] + "://" + m[2] + m[3]
	if query != "" {
		real += "?" + query
	}
	return real, true
}

// writeResponse streams/rewrites the upstream response back to the browser.
func (p *Proxy) writeResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, target *TargetInfo) {
	outHeader := w.Header()
	needsProcess := bodyNeedsProcess(resp)
	isRedirect := isRedirectStatus(resp.StatusCode)
	isHead := r.Method == http.MethodHead

	// Decompress on the fly before rewriting. When the encoding cannot be
	// decoded here (zstd, corrupt stream, unknown coding), the body must be
	// forwarded VERBATIM with its Content-Encoding intact - stripping the
	// header would make the browser render undecodable bytes (the upstream
	// relies on the same rule: only encodings it can decode are consumed).
	// Redirect bodies and HEAD responses are not forwarded, so there is
	// nothing to decode for them.
	var body io.Reader = resp.Body
	if needsProcess && !isRedirect && !isHead {
		if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
			// Record every byte the decoder constructors read. gzip.NewReader
			// consumes the header before it can report a corrupt stream; on
			// failure the fallback must replay those bytes or the response
			// would advertise the origin Content-Length while sending a
			// shorter body (the client then waits forever).
			rec := &recordingReader{r: body}
			if dec, ok := newDecoderChain(enc, rec); ok {
				body = io.NopCloser(dec)
			} else {
				needsProcess = false
				body = io.MultiReader(bytes.NewReader(rec.buf), body)
			}
		}
	}

	// HEAD has no body to rewrite, so keep the origin representation headers
	// (including Content-Length and Content-Encoding) exactly as sent. This
	// is what the upstream does and avoids advertising a compressed length
	// without its encoding.
	rewriteResponseHeaders(p.cfg, resp, outHeader, target, needsProcess && !isHead)

	if isHead {
		w.WriteHeader(resp.StatusCode)
		return
	}

	// Redirects never forward a body (upstream parity): only the status,
	// Location and representation headers reach the browser. Content-Type
	// stays as the origin sent it (304 is exempt from the text/plain
	// fallback applied to untyped bodies).
	if isRedirect {
		if needsProcess {
			p.addControlCookies(outHeader, target)
		}
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
		if _, err := io.CopyBuffer(dst, body, buf); err != nil {
			log.Printf("[siteproxy] stream upstream body for %s: %v", target.Host, err)
		}
		return
	}

	ct := contentType(resp.Header.Get("Content-Type"))
	var out []byte
	var truncated bool
	var renderErr error
	switch {
	case strings.HasPrefix(ct, "text/html"):
		out, truncated, renderErr = p.renderHTML(resp, body, target)
	case strings.HasPrefix(ct, "text/css"):
		out, truncated, renderErr = p.renderCSS(resp, body, target)
	default: // javascript and anything else text-ish
		out, truncated, renderErr = p.renderJS(resp, body, target)
	}
	if renderErr != nil {
		// The upstream body could not be read completely. Do not turn a
		// broken transfer into a successful 200 with a partial page.
		log.Printf("[siteproxy] read upstream body for %s: %v", target.Host, renderErr)
		clearResponseHeader(outHeader)
		http.Error(w, "Proxy fetch error", http.StatusBadGateway)
		return
	}
	if !truncated {
		// Only advertise UTF-8 when the rewritten bytes really are UTF-8.
		// For an undecodable legacy body (no header/meta charset) preserve
		// the upstream Content-Type so the browser can still sniff/guess
		// instead of being forced into mojibake.
		if utf8.Valid(out) {
			switch {
			case strings.HasPrefix(ct, "text/html"):
				outHeader.Set("Content-Type", "text/html; charset=UTF-8")
			case strings.HasPrefix(ct, "text/css"):
				outHeader.Set("Content-Type", "text/css; charset=UTF-8")
			default:
				outHeader.Set("Content-Type", ct+"; charset=UTF-8")
			}
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
			if cs != "" {
				if isUTF8(cs) {
					cs = "UTF-8"
				}
				outHeader.Set("Content-Type", "text/html; charset="+cs)
			} else if utf8.Valid(out) {
				// No declaration, but the bytes are valid UTF-8: declaring it
				// is safe and avoids the browser guessing a legacy charset.
				outHeader.Set("Content-Type", "text/html; charset=UTF-8")
			}
			// Otherwise keep the original Content-Type (no charset) so an
			// undeclared legacy body can still be sniffed by the browser.
			// Inject the client runtime when the encoding is ASCII-compatible
			// (the tag is pure ASCII, so GBK/Big5 bytes are untouched).
			// UTF-16 bodies cannot carry an ASCII snippet.
			if !isUTF16 {
				out = injectSnippetBytes(out, injectedScriptTagAt(p.cfg.BasePath()))
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(out)
		if _, err := io.Copy(w, body); err != nil {
			log.Printf("[siteproxy] stream upstream remainder for %s: %v", target.Host, err)
		}
		return
	}
	if len(out) == 0 {
		// Upstream parity: an empty body is not injected into and is
		// forwarded as-is (the original returns body=undefined). Keep the
		// upstream Content-Type exactly as sent instead of dropping it.
		if orig := resp.Header.Get("Content-Type"); orig != "" {
			outHeader.Set("Content-Type", orig)
		}
		outHeader.Set("Content-Length", "0")
		w.WriteHeader(resp.StatusCode)
		return
	}
	p.addControlCookies(outHeader, target)
	outHeader.Set("Content-Length", fmt.Sprint(len(out)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}

// addControlCookies adds the HttpOnly site-context cookies to responses whose
// body is actually rewritten (or to text redirects). Empty bodies, 204/304,
// HEAD and binary streams deliberately do not receive them: upstream does not
// set them there either, and they would otherwise create phantom session state.
func (p *Proxy) addControlCookies(h http.Header, target *TargetInfo) {
	if target == nil {
		return
	}
	h.Add("Set-Cookie", "proxy_real_protocol="+target.Protocol+"; Path=/; HttpOnly")
	h.Add("Set-Cookie", "proxy_real_host="+target.Host+"; Path=/; HttpOnly")
}

// clearResponseHeader removes every header already staged on w so that an
// error response after reading the upstream body does not leak control
// cookies or representation headers into the 502.
func clearResponseHeader(h http.Header) {
	for k := range h {
		delete(h, k)
	}
}

// recordingReader keeps a copy of every byte read from the wrapped reader.
// It is used while constructing decompression decoders: those constructors
// may consume a header before failing, and the verbatim fallback has to
// replay the consumed prefix.
type recordingReader struct {
	r   io.Reader
	buf []byte
}

func (rr *recordingReader) Read(p []byte) (int, error) {
	n, err := rr.r.Read(p)
	if n > 0 {
		rr.buf = append(rr.buf, p[:n]...)
	}
	return n, err
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
func (p *Proxy) renderHTML(resp *http.Response, body io.Reader, target *TargetInfo) ([]byte, bool, error) {
	raw, truncated, err := readAllLimited(body, maxRewriteBody)
	if err != nil {
		return nil, false, err
	}
	if truncated {
		return raw, true, nil
	}
	if len(raw) == 0 {
		return raw, false, nil
	}
	html, used := decodeHTMLBody(raw, resp.Header.Get("Content-Type"))
	// Rewrite the charset meta only when we actually produced UTF-8 (or the
	// input already was valid UTF-8). An undeclared legacy body must keep its
	// original meta so the browser can still sniff/guess it.
	rewriteCharset := used != "" || utf8.Valid(raw)
	base := p.cfg.ProxyURL + p.cfg.TokenPrefix + target.SitePrefix() + target.Path
	inject := renderInlineBootstrap(p.cfg, target.Protocol, target.Host) + injectedScriptTagAt(p.cfg.BasePath())
	return []byte(rewriteHTMLWithCharset(html, p.publicPrefix(), base, p.proxyHost(), inject, rewriteCharset)), false, nil
}

// renderCSS rewrites url(...) / @import inside stylesheets (upstream node
// pipeline).
func (p *Proxy) renderCSS(resp *http.Response, body io.Reader, target *TargetInfo) ([]byte, bool, error) {
	raw, truncated, err := readAllLimited(body, maxRewriteBody)
	if err != nil {
		return nil, false, err
	}
	if truncated {
		return raw, true, nil
	}
	if len(raw) == 0 {
		return raw, false, nil
	}
	txt, used := decodeTextBody(raw, resp.Header.Get("Content-Type"))
	// Normalise @charset whenever the bytes really are UTF-8, even if the
	// HTTP header (not the @charset) was what told us so. Leaving a stale
	// "@charset gbk" in a UTF-8 stylesheet makes the browser re-decode it.
	if used != "" || utf8.Valid(txt) {
		txt = rewriteCSSCharset(txt)
	}
	base := p.cfg.ProxyURL + p.cfg.TokenPrefix + target.SitePrefix() + target.Path
	return []byte(rewriteCSS(string(txt), p.publicPrefix(), base, p.proxyHost())), false, nil
}

// renderJS rewrites the window.location assignment forms inside JavaScript
// files (upstream node shape) and prepends the ___location bootstrap line.
// Static string literals are intentionally left untouched - the injected
// client runtime traps fetch / XHR / sendBeacon at call time instead.
func (p *Proxy) renderJS(resp *http.Response, body io.Reader, target *TargetInfo) ([]byte, bool, error) {
	raw, truncated, err := readAllLimited(body, maxRewriteBody)
	if err != nil {
		return nil, false, err
	}
	if truncated {
		return raw, true, nil
	}
	if len(raw) == 0 {
		return raw, false, nil
	}
	txt, _ := decodeTextBody(raw, resp.Header.Get("Content-Type"))
	return []byte(jsBootstrapLine + rewriteJSFile(string(txt))), false, nil
}

// ----------------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------------

// maxRewriteBody caps how much of a text body we buffer for rewriting
// (16 MiB). Larger bodies are streamed untouched.
const maxRewriteBody = 16 << 20

// readAllLimited reads r fully, reporting whether the limit was exceeded.
// Any read error is returned to the caller: a truncated upstream transfer
// must not be rewritten and served as a successful partial page.
func readAllLimited(r io.Reader, limit int64) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return data, false, err
	}
	return data, int64(len(data)) > limit, nil
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
	// A 206 body is a byte range, not a document: rewriting it would both
	// corrupt the payload and advertise a Content-Length that no longer
	// matches Content-Range. Stream ranges verbatim.
	if resp.StatusCode == http.StatusPartialContent || resp.Header.Get("Content-Range") != "" {
		return false
	}
	if resp.StatusCode >= 500 {
		return false // upstream keeps error bodies verbatim (status < 500 rewrite)
	}
	// A download is a byte-for-byte artifact, not a navigable document.
	// Injecting the runtime into it would change the file the user saves.
	if cd := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Disposition"))); strings.HasPrefix(cd, "attachment") {
		return false
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

// isProxyOrigin reports whether origin is this proxy. The configured public
// URL is authoritative, but a reverse proxy / test setup may expose the
// service under a different host; the inbound Host is then the second
// candidate. Without this, `Origin: <proxy>` can leak upstream.
func (p *Proxy) isProxyOrigin(origin string, r *http.Request) bool {
	if sameOrigin(origin, p.cfg.ProxyURL) {
		return true
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return sameOrigin(origin, scheme+"://"+r.Host)
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
