package main

import (
        "net/http"
        "regexp"
        "strings"
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
                case "location", "refresh", "set-cookie":
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

        // Set-Cookie: isolate per site by rewriting Path and dropping Domain.
        // (Cookie paths are always path-only: token prefix + site prefix.)
        for _, sc := range resp.Header.Values("Set-Cookie") {
                out.Add("Set-Cookie", rewriteSetCookie(sc, cfg.TokenPrefix, sitePrefix, cfg))
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
        switch {
        case strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "http://"):
                return RewriteURL(loc, publicPrefix, proxyHost)
        case strings.HasPrefix(loc, "//"):
                return publicPrefix + "https/" + loc[2:]
        case strings.HasPrefix(loc, "/"):
                return publicPrefix + sitePrefix + loc
        default:
                // Relative Location (RFC 7231): leave for the browser to resolve
                // against the current proxied path.
                return loc
        }
}

var refreshURLRe = regexp.MustCompile(`(?i)^(.*?url\s*=\s*)(.+)$`)

// rewriteRefreshHeader rewrites the URL inside a "Refresh: 2; url=/path".
func rewriteRefreshHeader(ref, publicPrefix, sitePrefix, proxyHost string) string {
        m := refreshURLRe.FindStringSubmatch(ref)
        if m == nil {
                return ref
        }
        u := strings.Trim(m[2], `"'`)
        var rewritten string
        if strings.HasPrefix(u, "/") {
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
func rewriteSetCookie(sc, token, sitePrefix string, cfg *Config) string {
        parts := strings.Split(sc, ";")
        if len(parts) == 0 {
                return sc
        }
        kv := strings.TrimSpace(parts[0])
        if kv == "" || kv == "=" {
                return sc
        }

        // Extract attributes, dropping/rewriting the isolation-relevant ones.
        origPath := "/"
        attrs := make([]string, 0, len(parts))
        for _, p := range parts[1:] {
                p = strings.TrimSpace(p)
                if p == "" {
                        continue
                }
                lower := strings.ToLower(p)
                switch {
                case strings.HasPrefix(lower, "domain="):
                        // drop: cookies become host-only on the proxy domain
                case strings.HasPrefix(lower, "path="):
                        if v := p[len("path="):]; v != "" {
                                origPath = v
                        }
                case lower == "secure":
                        if proxyIsHTTPS(cfg) {
                                attrs = append(attrs, "Secure")
                        }
                case strings.HasPrefix(lower, "samesite="):
                        attrs = append(attrs, "SameSite=Lax")
                default:
                        attrs = append(attrs, p)
                }
        }

        out := kv + "; Path=" + token + sitePrefix + origPath
        for _, a := range attrs {
                out += "; " + a
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
                if name == "proxy_real_host" || name == "proxy_real_protocol" {
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
