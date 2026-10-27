package main

import (
	"net/url"
	"regexp"
	"strings"
)

// ----------------------------------------------------------------------------
// URL scheme conversion.
//
// Original siteproxy path format (see the deobfuscated proxyRewrite):
//
//      {token_prefix}{http|https}/{host}/{rest}?{query}
//
// e.g. with token_prefix "/user22334455/":
//
//      /user22334455/https/www.wikipedia.org/  ->  https://www.wikipedia.org/
// ----------------------------------------------------------------------------

var (
	// pathPrefixRe extracts protocol and host from a proxied path:
	// /?(http[s]?)/([^/]+)(?:/(.*))?
	// The leading slash is optional because the token prefix (which ends
	// with "/") is stripped before parsing.
	pathPrefixRe = regexp.MustCompile(`^/?(https?)/([^/]+)(/.*)?$`)
)

// TargetInfo describes a proxied request target.
type TargetInfo struct {
	Protocol string // "http" or "https"
	Host     string // host[:port]
	Path     string // always starts with "/", "/" when empty
	RawQuery string
}

// ParseTarget parses a request path (already stripped of token_prefix) into a
// TargetInfo. Returns nil when the path is not a valid proxy target.
func ParseTarget(path, rawQuery string) *TargetInfo {
	// Defensive: drop a query string if the caller included it in path.
	if i := strings.IndexByte(path, '?'); i >= 0 {
		if rawQuery == "" {
			rawQuery = path[i+1:]
		}
		path = path[:i]
	}
	m := pathPrefixRe.FindStringSubmatch(path)
	if m == nil {
		return nil
	}
	host := m[2]
	if !isPlausibleHost(host) {
		return nil
	}
	rest := m[3]
	if rest == "" {
		rest = "/"
	}
	return &TargetInfo{
		Protocol: m[1],
		Host:     host,
		Path:     rest,
		RawQuery: rawQuery,
	}
}

// URL builds the upstream URL string ("https://host/path?query").
func (t *TargetInfo) URL() string {
	u := t.Protocol + "://" + t.Host + t.Path
	if t.RawQuery != "" {
		u += "?" + t.RawQuery
	}
	return u
}

// SitePrefix returns the proxy path prefix that all resources of this target
// share: "{proto}/{host}". Cookie isolation keys on this prefix.
func (t *TargetInfo) SitePrefix() string {
	return t.Protocol + "/" + t.Host
}

// ----------------------------------------------------------------------------
// Rewriting helpers (server side counterparts of the client proxyRewrite()).
// ----------------------------------------------------------------------------

// hostOfURL returns the host part of an absolute http(s) URL, "" otherwise.
func hostOfURL(u string) string {
	i := strings.Index(u, "://")
	if i < 0 {
		return ""
	}
	rest := u[i+3:]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		return rest[:j]
	}
	return rest
}

// RewriteURL converts an absolute http(s) URL into its proxied ABSOLUTE URL
// (same output shape as the original siteproxy):
//
//	https://x.com/a -> {proxy_url}{token_prefix}https/x.com/a
//
// publicPrefix must be "{proxy_url}{token_prefix}" (proxy_url has no
// trailing slash, token_prefix starts and ends with "/").
// Relative URLs are returned unchanged (they are handled with site context by
// RewriteWithBase / the injected client scripts).
// URLs pointing at the proxy's own host are returned unchanged to avoid
// self-referential loops (v2.7.4).
func RewriteURL(rawURL, publicPrefix, proxyHost string) string {
	if rawURL == "" {
		return rawURL
	}
	if strings.HasPrefix(strings.ToLower(rawURL), strings.ToLower(publicPrefix)) {
		return rawURL // already proxied (case-insensitive scheme/host)
	}
	if proxyHost != "" && strings.EqualFold(hostOfURL(rawURL), proxyHost) {
		return rawURL
	}
	lower := strings.ToLower(rawURL)
	if strings.HasPrefix(lower, "https://") {
		return publicPrefix + "https/" + rawURL[len("https://"):]
	}
	if strings.HasPrefix(lower, "http://") {
		return publicPrefix + "http/" + rawURL[len("http://"):]
	}
	return rawURL
}

// RewriteWithBase rewrites any URL (absolute, protocol-relative, root- or
// path-relative) into a fully proxied ABSOLUTE URL, using base to resolve
// relative references. base must be a proxied absolute URL
// ({proxy_host}{token_prefix}{proto}/{host}/...).
func RewriteWithBase(rawURL, publicPrefix, baseProxiedURL, proxyHost string) string {
	if rawURL == "" {
		return rawURL
	}
	if rawURL == "" || strings.ContainsAny(rawURL, " \t\r\n\f\v") {
		// Empty / whitespace-containing attribute values are not URLs; the
		// upstream regexes deliberately do not rewrite them.
		return rawURL
	}
	trimmed := rawURL
	lower := strings.ToLower(trimmed)
	if hasUnsupportedScheme(trimmed) {
		return rawURL
	}
	if tokenPrefix := tokenPrefixOf(publicPrefix); tokenPrefix != "" && tokenPrefix != "/" &&
		strings.HasPrefix(trimmed, tokenPrefix) {
		// Already inside the proxy scope (but without the absolute proxy
		// host): never wrap it a second time.
		return rawURL
	}
	switch {
	// Do not touch non-http schemes.
	case strings.HasPrefix(lower, "javascript:") ||
		strings.HasPrefix(lower, "data:") ||
		strings.HasPrefix(lower, "blob:") ||
		strings.HasPrefix(lower, "mailto:") ||
		strings.HasPrefix(lower, "tel:") ||
		strings.HasPrefix(lower, "about:") ||
		strings.HasPrefix(lower, "ws://") ||
		strings.HasPrefix(lower, "wss://") ||
		strings.HasPrefix(trimmed, "#"):
		return rawURL
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		return RewriteURL(trimmed, publicPrefix, proxyHost)
	case strings.HasPrefix(trimmed, "//"):
		host := hostOfURL("https:" + trimmed)
		if host == "" {
			return rawURL
		}
		if proxyHost != "" && strings.EqualFold(host, proxyHost) {
			return rawURL // protocol-relative URL pointing at the proxy itself
		}
		return publicPrefix + "https/" + trimmed[2:]
	case strings.HasPrefix(trimmed, "/"):
		proto, host := baseOf(baseProxiedURL, publicPrefix)
		if host == "" {
			return rawURL
		}
		return publicPrefix + proto + "/" + host + trimmed
	default:
		// Resolve dot segments, query-only references (?page=2) and relative
		// paths exactly like a browser would.
		if base, err := url.Parse(baseProxiedURL); err == nil && base.IsAbs() {
			if ref, err := url.Parse(trimmed); err == nil {
				return base.ResolveReference(ref).String()
			}
		}
		proto, host := baseOf(baseProxiedURL, publicPrefix)
		if host == "" {
			return rawURL
		}
		basePath := pathOf(baseProxiedURL, publicPrefix)
		dir := "/"
		if i := strings.LastIndex(basePath, "/"); i >= 0 {
			dir = basePath[:i+1]
		}
		return publicPrefix + proto + "/" + host + dir + trimmed
	}
}

// hasUnsupportedScheme reports whether rawURL uses a non-http(s) scheme (ftp:,
// sms:, file:, chrome-extension:, vbscript:, ...). Those are returned
// unchanged, like the original server does for every `^[a-z]+:` value.
func hasUnsupportedScheme(rawURL string) bool {
	colon := strings.IndexByte(rawURL, ':')
	if colon <= 0 {
		return false
	}
	if slash := strings.IndexAny(rawURL, "/?#"); slash >= 0 && slash < colon {
		return false
	}
	scheme := rawURL[:colon]
	for i := 0; i < len(scheme); i++ {
		c := scheme[i]
		alpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !alpha && !(i > 0 && ((c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.')) {
			return false
		}
	}
	switch strings.ToLower(scheme) {
	case "http", "https":
		// "http:foo" is not a web URL and must not be treated as a relative
		// path; only scheme:// forms are proxied.
		return !strings.HasPrefix(strings.ToLower(rawURL), strings.ToLower(scheme)+"://")
	}
	return true
}

// tokenPrefixOf extracts the token part from "{proxy_url}{token_prefix}".
func tokenPrefixOf(publicPrefix string) string {
	i := strings.Index(publicPrefix, "://")
	if i < 0 {
		return ""
	}
	rest := publicPrefix[i+3:]
	j := strings.IndexByte(rest, '/')
	if j < 0 {
		return ""
	}
	return rest[j:]
}

// UnrewriteURL converts a proxied URL back into the real target URL. Used for
// Referer / Origin reconstruction.
func UnrewriteURL(u, tokenPrefix string) string {
	return UnrewriteURLWithBase(u, tokenPrefix, "")
}

// UnrewriteURLWithBase is UnrewriteURL for a proxy deployed under basePath
// (e.g. "/sub"). The base path is stripped before the token prefix.
func UnrewriteURLWithBase(u, tokenPrefix, basePath string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return u
	}
	// EscapedPath keeps %2F and other encoded bytes intact; parsed.Path
	// would decode them and change the real target URL.
	rest := parsed.EscapedPath()
	if basePath != "" && (rest == basePath || strings.HasPrefix(rest, basePath+"/")) {
		rest = strings.TrimPrefix(rest, basePath)
		if rest == "" {
			rest = "/"
		}
	}
	if tokenPrefix != "/" {
		if !strings.HasPrefix(rest, tokenPrefix) {
			return u
		}
		rest = rest[len(tokenPrefix):]
	}
	t := ParseTarget(rest, parsed.RawQuery)
	if t == nil {
		return u
	}
	return t.URL()
}

// baseOf extracts "proto/host" from a proxied absolute URL.
// publicPrefix is "{proxy_url}{token_prefix}"; the token part is derived from it.
func baseOf(proxied, publicPrefix string) (proto, host string) {
	i := strings.Index(proxied, "://")
	if i < 0 {
		return "", ""
	}
	rest := proxied[i+3:] // host + token_prefix + proto/host/...
	if j := strings.Index(rest, "/"); j >= 0 {
		rest = rest[j:] // keep the leading slash: "/token_prefix/proto/..."
	} else {
		return "", ""
	}
	tokenPrefix := ""
	if k := strings.Index(publicPrefix, "://"); k >= 0 {
		if j := strings.Index(publicPrefix[k+3:], "/"); j >= 0 {
			tokenPrefix = publicPrefix[k+3+j:]
		}
	}
	if tokenPrefix != "/" && tokenPrefix != "" {
		if !strings.HasPrefix(rest, tokenPrefix) {
			return "", ""
		}
		rest = rest[len(tokenPrefix):]
	}
	m := pathPrefixRe.FindStringSubmatch(rest)
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}

// pathOf extracts the target path part from a proxied absolute URL.
func pathOf(proxied, publicPrefix string) string {
	i := strings.Index(proxied, "://")
	if i < 0 {
		return "/"
	}
	rest := proxied[i+3:]
	j := strings.Index(rest, "/")
	if j < 0 {
		return "/"
	}
	rest = rest[j:] // "/token_prefix/proto/host/path"
	tokenPrefix := ""
	if k := strings.Index(publicPrefix, "://"); k >= 0 {
		if j2 := strings.Index(publicPrefix[k+3:], "/"); j2 >= 0 {
			tokenPrefix = publicPrefix[k+3+j2:]
		}
	}
	if tokenPrefix != "/" && tokenPrefix != "" {
		if !strings.HasPrefix(rest, tokenPrefix) {
			return "/"
		}
		rest = rest[len(tokenPrefix):]
	}
	m := pathPrefixRe.FindStringSubmatch(rest)
	if m == nil || m[3] == "" {
		return "/"
	}
	return m[3]
}
