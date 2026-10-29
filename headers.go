package main

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Headers that must never cross to the other side of the proxy.
var (
	// Hop-by-hop headers (RFC 7230 §6.1).
	hopByHopHeaders = []string{
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade", "Proxy-Connection",
	}

	// Infrastructure / edge headers that would leak the proxy's own presence.
	// Mirrors the upstream v2.7.11 filter set; the cf-* / x-forwarded* /
	// siteproxy* families are matched by prefix in filterHopHeaders so that
	// new edge headers (e.g. cf-ipcity) are covered automatically.
	infraHeaders = []string{
		"Host",
		"Cdn-Loop", "True-Client-Ip",
		"X-Real-Ip", "X-Client-Ip", "X-Cluster-Client-Ip",
		"Fastly-Client-Ip", "Fly-Client-Ip",
		"Forwarded", "Via",
		"X-Request-Start", "X-Served-By",
	}

	// Internal siteproxy headers (also matched by the siteproxy* prefix).
	internalHeaders = []string{
		"X-Siteproxy-Body-Headers",
	}

	// Security headers that would block our injection or framing.
	stripResponseHeaders = []string{
		"Content-Security-Policy", "Content-Security-Policy-Report-Only",
		"Cross-Origin-Opener-Policy", "Cross-Origin-Opener-Policy-Report-Only",
		"Cross-Origin-Embedder-Policy", "Cross-Origin-Embedder-Policy-Report-Only",
		"Cross-Origin-Resource-Policy",
		"X-Frame-Options", "Permissions-Policy", "Report-To",
		"Reporting-Endpoints", "X-Xss-Protection",
		"Nel", // Network Error Logging points at the upstream origin
		"Clear-Site-Data", "Public-Key-Pins",
		"Alt-Svc", "Alt-Used", // never advertise the upstream origin
	}
)

// headerMatchesFilter reports whether a header name must be dropped. It
// reproduces the upstream v2.7.11 filter: prefix families siteproxy / cf- /
// x-forwarded (case-insensitive) plus an exact-match set of edge headers.
func headerMatchesFilter(lowerName string) bool {
	for _, p := range []string{"siteproxy", "cf-", "x-forwarded"} {
		if strings.HasPrefix(lowerName, p) {
			return true
		}
	}
	for _, h := range hopByHopHeaders {
		if lowerName == strings.ToLower(h) {
			return true
		}
	}
	for _, h := range infraHeaders {
		if lowerName == strings.ToLower(h) {
			return true
		}
	}
	for _, h := range internalHeaders {
		if lowerName == strings.ToLower(h) {
			return true
		}
	}
	return false
}

// copyUpstreamHeaders copies allowed request headers to the upstream request.
func copyUpstreamHeaders(dst, src http.Header) {
	// Managed separately by the proxy pipeline:
	skip := map[string]bool{
		"accept-encoding": true,
		"cookie":          true,
		"referer":         true,
		"origin":          true,
		"host":            true,
		"content-length":  true, // transport derives it from ContentLength
	}

	// NOTE: headers merely NAMED in the Connection header are NOT dropped
	// here — the original node server forwards them (verified by
	// differential testing), and browsers never send exotic ones.

	for name, vals := range src {
		lower := strings.ToLower(name)
		if skip[lower] || headerMatchesFilter(lower) {
			continue
		}
		for _, v := range vals {
			dst.Add(name, v)
		}
	}
}

// rewriteResponseHeaders adjusts upstream response headers for the browser.
// When the body pipeline rewrites the payload, Content-Encoding /
// Content-Length are dropped (the Go http server re-chunks and re-counts).
func rewriteResponseHeaders(cfg *Config, resp *http.Response, out http.Header, target *TargetInfo, bodyProcessed bool) {
	strip := map[string]bool{}
	for _, h := range stripResponseHeaders {
		strip[strings.ToLower(h)] = true
	}
	for _, h := range infraHeaders {
		strip[strings.ToLower(h)] = true
	}
	if bodyProcessed {
		strip["content-encoding"] = true
		strip["content-length"] = true
	}

	for name, vals := range resp.Header {
		lower := strings.ToLower(name)
		if strip[lower] {
			continue
		}
		// Same prefix families the upstream server deletes from responses.
		if headerMatchesFilter(lower) {
			continue
		}
		switch strings.ToLower(name) {
		case "location", "refresh", "set-cookie", "link":
			continue // handled below
		}
		for _, v := range vals {
			out.Add(name, v)
		}
	}

	publicPrefix := cfg.ProxyURL + cfg.TokenPrefix
	sitePrefix := ""
	if target != nil {
		sitePrefix = target.SitePrefix()
	}

	// Location: absolute -> proxied path; relative -> site-scoped path.
	if loc := resp.Header.Get("Location"); loc != "" {
		out.Set("Location", rewriteRedirect(loc, publicPrefix, sitePrefix, hostOfURL(cfg.ProxyURL)))
	}
	if ref := resp.Header.Get("Refresh"); ref != "" {
		out.Set("Refresh", rewriteRefreshHeader(ref, publicPrefix, sitePrefix, hostOfURL(cfg.ProxyURL)))
	}
	// Link: <https://origin/a>; rel=preload must not make the browser fetch
	// the real origin directly. Rewrite every <...> URL into proxy space.
	for _, link := range resp.Header.Values("Link") {
		out.Add("Link", rewriteLinkHeader(link, publicPrefix, sitePrefix, hostOfURL(cfg.ProxyURL)))
	}

	// Set-Cookie: isolate per site by rewriting Path and dropping Domain.
	// Some origins fold several cookies into one header line (technically
	// invalid, but common); split them like the upstream node server does
	// while leaving the comma in Expires dates alone.
	for _, sc := range resp.Header.Values("Set-Cookie") {
		for _, one := range splitSetCookieHeader(sc) {
			if rewritten := rewriteSetCookie(one, cfg.TokenPrefix, sitePrefix, cfg); rewritten != "" {
				out.Add("Set-Cookie", rewritten)
			}
		}
	}

	// Upstream parity: the node pipeline defaults a missing content-type
	// to text/plain; browsers then never sniff an untyped body as HTML.
	if out.Get("Content-Type") == "" && resp.Header.Get("Content-Type") == "" {
		out.Set("Content-Type", "text/plain; charset=UTF-8")
	}
}

// rewriteRedirect maps a Location header value into proxy space (absolute
// URLs, matching the original siteproxy output shape).
func rewriteRedirect(loc, publicPrefix, sitePrefix, proxyHost string) string {
	lower := strings.ToLower(loc)
	if tp := tokenPrefixOf(publicPrefix); tp != "" && tp != "/" && strings.HasPrefix(loc, tp) {
		return loc // already in proxy scope
	}
	switch {
	case strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "http://"):
		return RewriteURL(loc, publicPrefix, proxyHost)
	case strings.HasPrefix(loc, "//"):
		if proxyHost != "" && strings.EqualFold(hostOfURL("https:"+loc), proxyHost) {
			return loc
		}
		return publicPrefix + "https/" + loc[2:]
	case strings.HasPrefix(loc, "/"):
		return publicPrefix + sitePrefix + loc
	default:
		// Relative Location (RFC 7231): leave for the browser to resolve
		// against the current proxied path.
		return loc
	}
}

var (
	refreshURLRe = regexp.MustCompile(`(?i)^(.*?url\s*=\s*)(.+)$`)
	linkURLRe    = regexp.MustCompile(`<([^>]+)>`)
)

// rewriteLinkHeader rewrites the <...> URLs of a Link response header.
func rewriteLinkHeader(v, publicPrefix, sitePrefix, proxyHost string) string {
	return linkURLRe.ReplaceAllStringFunc(v, func(m string) string {
		g := linkURLRe.FindStringSubmatch(m)
		return "<" + rewriteLinkURL(g[1], publicPrefix, sitePrefix, proxyHost) + ">"
	})
}

func rewriteLinkURL(u, publicPrefix, sitePrefix, proxyHost string) string {
	lower := strings.ToLower(u)
	switch {
	case strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "http://"):
		return RewriteURL(u, publicPrefix, proxyHost)
	case strings.HasPrefix(u, "//"):
		if proxyHost != "" && strings.EqualFold(hostOfURL("https:"+u), proxyHost) {
			return u
		}
		return publicPrefix + "https/" + u[2:]
	case strings.HasPrefix(u, "/"):
		return publicPrefix + sitePrefix + u
	default:
		return u
	}
}

// rewriteRefreshHeader rewrites the URL inside a "Refresh: 2; url=/path".
func rewriteRefreshHeader(ref, publicPrefix, sitePrefix, proxyHost string) string {
	m := refreshURLRe.FindStringSubmatch(ref)
	if m == nil {
		return ref
	}
	u := strings.Trim(m[2], `"'`)
	if tp := tokenPrefixOf(publicPrefix); tp != "" && tp != "/" && strings.HasPrefix(u, tp) {
		return ref // already in proxy scope
	}
	var rewritten string
	if strings.HasPrefix(u, "//") {
		if proxyHost != "" && strings.EqualFold(hostOfURL("https:"+u), proxyHost) {
			return ref
		}
		rewritten = publicPrefix + "https/" + u[2:]
	} else if strings.HasPrefix(u, "/") {
		rewritten = publicPrefix + sitePrefix + u
	} else {
		rewritten = RewriteURL(u, publicPrefix, proxyHost)
	}
	return m[1] + rewritten
}

// rewriteSetCookie performs v2.6.0-style cookie isolation:
//   - Domain= is dropped so the cookie lands on the proxy host;
//   - Path= is rewritten to {token_prefix}{proto}/{host}{orig-path} so each
//     website's cookies live in their own path scope and can never collide
//     (this is what prevents the classic "Cookie header > 8K -> 502" bug);
//   - Secure is dropped when the proxy itself is plain HTTP, otherwise the
//     browser would silently discard the cookie;
//   - SameSite is normalised to Lax so the cookie works across the proxy
//     origin without needing the Secure attribute.
//
// splitSetCookieHeader splits a folded Set-Cookie header on commas that do
// not belong to an Expires date ("Thu, 01 Jan 1970 ..."). It mirrors the
// upstream node regex `,(?!(?:\s+[0-9]{2}))`.
func splitSetCookieHeader(v string) []string {
	var out []string
	start := 0
	for i := 0; i < len(v); i++ {
		if v[i] != ',' {
			continue
		}
		j := i + 1
		for j < len(v) && (v[j] == ' ' || v[j] == '\t') {
			j++
		}
		if j+1 < len(v) && v[j] >= '0' && v[j] <= '9' && v[j+1] >= '0' && v[j+1] <= '9' {
			continue // Expires date comma
		}
		out = append(out, strings.TrimSpace(v[start:i]))
		start = i + 1
	}
	out = append(out, strings.TrimSpace(v[start:]))
	return out
}

func rewriteSetCookie(sc, token, sitePrefix string, cfg *Config) string {
	parts := strings.Split(sc, ";")
	if len(parts) == 0 {
		return ""
	}
	kv := strings.TrimSpace(parts[0])
	if kv == "" || kv == "=" || !strings.Contains(kv, "=") {
		return ""
	}
	name := kv
	if i := strings.IndexByte(kv, '='); i >= 0 {
		name = kv[:i]
	}
	isControl := name == "proxy_real_host" || name == "proxy_real_protocol"
	capLifetime := cfg.CookieMaxAge > 0 && !isControl
	hasPastExpires := false
	hasDeleteMaxAge := false

	// Extract attributes, dropping/rewriting the isolation-relevant ones.
	// Attribute names are parsed with optional whitespace around '=' (e.g.
	// "Path = /", "Domain = .example.com"): browsers accept that form, so a
	// prefix-only match would let the isolation attributes bypass rewriting.
	origPath := "/"
	attrs := make([]string, 0, len(parts))
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		name, val := p, ""
		if i := strings.IndexByte(p, '='); i >= 0 {
			name = strings.TrimSpace(p[:i])
			val = strings.TrimSpace(p[i+1:])
		}
		switch strings.ToLower(name) {
		case "domain":
			// drop: cookies become host-only on the proxy domain
		case "max-age":
			n, err := strconv.Atoi(val)
			if err == nil && n <= 0 {
				hasDeleteMaxAge = true
			}
			if capLifetime && !(err == nil && n <= 0) {
				continue // replace a positive/malformed Max-Age with the cap
			}
			attrs = append(attrs, p)
		case "expires":
			if capLifetime {
				if t, err := http.ParseTime(val); err == nil {
					if !t.Before(time.Now()) {
						continue // replace future expiry with the cap
					}
					hasPastExpires = true
				}
			}
			attrs = append(attrs, p)
		case "path":
			if val != "" {
				origPath = val
			}
		case "secure":
			if proxyIsHTTPS(cfg) {
				attrs = append(attrs, "Secure")
			}
		case "samesite":
			// HTTPS deployments can keep None/Strict unchanged. On a plain
			// HTTP proxy, SameSite=None is rejected by browsers without
			// Secure, so normalise only that case to Lax.
			if proxyIsHTTPS(cfg) || !strings.EqualFold(val, "none") {
				attrs = append(attrs, p)
			} else {
				attrs = append(attrs, "SameSite=Lax")
			}
		default:
			attrs = append(attrs, p)
		}
	}

	// A Path value without a leading slash is treated as absolute by the
	// upstream (it prepends "/"); do the same so the isolation prefix is
	// always well formed.
	if !strings.HasPrefix(origPath, "/") {
		origPath = "/" + origPath
	}

	base := cfg.BasePath() + token + sitePrefix
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	baseNoSlash := strings.TrimSuffix(base, "/")
	outPath := origPath
	if origPath == "/" {
		outPath = base
	} else if origPath != baseNoSlash &&
		!strings.HasPrefix(origPath, base) &&
		!(strings.HasPrefix(origPath, baseNoSlash) && len(origPath) > len(baseNoSlash) && origPath[len(baseNoSlash)] == '/') {
		outPath = baseNoSlash + origPath
	}
	out := kv + "; Path=" + outPath
	for _, a := range attrs {
		out += "; " + a
	}
	if capLifetime && !hasPastExpires && !hasDeleteMaxAge {
		out += "; Max-Age=" + strconv.Itoa(cfg.CookieMaxAge)
	}
	return out
}

// filterControlCookies removes the siteproxy control cookies
// (proxy_real_host / proxy_real_protocol) from an inbound Cookie header,
// exactly like the original server does before forwarding upstream. These
// cookies are scoped to Path=/ on the proxy origin and must never leak to
// proxied sites (they would also cross-contaminate targets).
func filterControlCookies(cookieHeader string) string {
	parts := strings.Split(cookieHeader, ";")
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		name := p
		if i := strings.IndexByte(p, '='); i >= 0 {
			name = p[:i]
		}
		if strings.HasPrefix(name, "proxy_real_") {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(kept, "; ")
}

// proxyIsHTTPS reports whether the public proxy URL is https.
func proxyIsHTTPS(cfg *Config) bool {
	return strings.HasPrefix(strings.ToLower(cfg.ProxyURL), "https://")
}

// ----------------------------------------------------------------------------
// websocket helpers
// ----------------------------------------------------------------------------

func isWebSocketUpgrade(r *http.Request) bool {
	for _, v := range r.Header.Values("Connection") {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "upgrade") {
				for _, u := range r.Header.Values("Upgrade") {
					if strings.EqualFold(strings.TrimSpace(u), "websocket") {
						return true
					}
				}
			}
		}
	}
	return false
}
