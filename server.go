package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

// Server is the top level HTTP handler implementing the siteproxy routing:
//
//	/                                  -> homepage
//	/index.html                        -> homepage
//	/siteproxy-response-injected.js    -> client runtime script
//	/siteproxy_service_worker.js       -> client service worker
//	{token_prefix}{proto}/{host}/...   -> proxied target
//	anything else                      -> 404 (or the token-scoped homepage)
type Server struct {
	cfg    *Config
	proxy  *Proxy
	client *http.Client

	homepageHTML []byte
}

// NewServer wires the routing table and prepares the embedded assets.
func NewServer(cfg *Config) (*Server, error) {
	client := newHTTPClient()
	p, err := NewProxy(cfg, client)
	if err != nil {
		return nil, err
	}

	return &Server{
		cfg:          cfg,
		proxy:        p,
		client:       client,
		homepageHTML: renderHomepage(cfg),
	}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	pr := &panicRecorder{ResponseWriter: w}
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[siteproxy] recovered from panic on %s %s: %v\n%s",
				r.Method, r.URL.Path, rec, debug.Stack())
			if !pr.wroteHeader {
				// Nothing sent yet: answer with a clean 502.
				http.Error(pr, "siteproxy: internal error", http.StatusBadGateway)
			}
		}
	}()
	s.serve(pr, r)
}

// panicRecorder tracks whether the response headers have been written so a
// recovered panic can still produce a 502. It transparently forwards the
// Flusher / Hijacker capabilities (WebSocket and SSE depend on them).
type panicRecorder struct {
	http.ResponseWriter
	wroteHeader bool
}

func (p *panicRecorder) WriteHeader(code int) {
	p.wroteHeader = true
	p.ResponseWriter.WriteHeader(code)
}

func (p *panicRecorder) Write(b []byte) (int, error) {
	p.wroteHeader = true
	return p.ResponseWriter.Write(b)
}

func (p *panicRecorder) Flush() {
	p.wroteHeader = true
	if f, ok := p.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (p *panicRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := p.ResponseWriter.(http.Hijacker); ok {
		p.wroteHeader = true
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("siteproxy: response writer does not support hijacking")
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()

	// Upstream serves the client runtime for any path ending in this name,
	// not just the root/base-path spelling. Keep that behaviour so a page
	// that asks for an in-scope copy (or an old cached URL shape) still gets
	// the runtime instead of a 404 or the target site's file.
	if strings.HasSuffix(path, "/siteproxy-response-injected.js") {
		s.serveInjectedScript(w, r)
		return
	}

	// A proxy mounted under a base path (proxy_url = https://host/sub) only
	// serves URLs inside that prefix. Without this guard the same proxy is
	// reachable both as /sub/{token}/... and /{token}/..., which breaks path
	// isolation and generated-link consistency.
	if bp := s.cfg.BasePath(); bp != "" && path != "/" && path != bp && !strings.HasPrefix(path, bp+"/") {
		s.serveHomepage(w, r)
		return
	}

	switch path {
	case "/", "/index.html", "/index.htm":
		s.serveHomepage(w, r)
		return
	case "/siteproxy_service_worker.js":
		// Rendered with the registration context. The registration URL must
		// carry the configured token; a bare public fetch is rejected.
		s.serveServiceWorker(w, r)
		return
	}

	// Paths may be mounted under a base path (proxy_url with a path).
	proxyPath := path
	if bp := s.cfg.BasePath(); bp != "" && (proxyPath == bp || strings.HasPrefix(proxyPath, bp+"/")) {
		proxyPath = strings.TrimPrefix(proxyPath, bp)
		if proxyPath == "" {
			proxyPath = "/"
		}
	}

	// Also honour the special endpoints when the proxy is mounted under a
	// base path (proxy_url = https://host/sub).
	switch proxyPath {
	case "/", "/index.html", "/index.htm":
		s.serveHomepage(w, r)
		return
	case "/siteproxy_service_worker.js":
		s.serveServiceWorker(w, r)
		return
	}

	// A direct /favicon.ico request has no site context only in no-password
	// mode. With a token, let it fall through to repair so a proxied page's
	// favicon is fetched from its real site (upstream behaviour).
	if proxyPath == "/favicon.ico" && s.cfg.noPassword() {
		http.NotFound(w, r)
		return
	}

	// Upstream `_0x41151c`: a token prefix may appear in the middle of a
	// path when a page mangles its own links or the Service Worker is not in
	// control. Re-anchor the target to the first valid `{token}{proto}/{host}`
	// sequence found after the leading token (if any).
	if target := s.targetFromPathToken(proxyPath, r.URL.RawQuery); target != nil {
		s.proxy.ServeHTTP(w, r, target)
		return
	}

	// Token (password) gate.
	if !s.cfg.noPassword() && !strings.HasPrefix(proxyPath, s.cfg.TokenPrefix) {
		// Upstream behaviour: a path outside the token scope is still
		// proxied when the request can be attributed to a proxied site
		// via Referer or the control cookies (both require knowledge
		// of the proxy session). Otherwise 404 (or the upstream
		// default_password redirect) - never leak the token-bearing
		// homepage to an unauthenticated request.
		if repaired := s.repairOutOfScope(r); repaired != nil {
			s.proxy.ServeHTTP(w, r, repaired)
			return
		}
		if s.cfg.DefaultPassword {
			s.redirectDefaultPassword(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}

	rest := strings.TrimPrefix(proxyPath, s.cfg.TokenPrefix)
	target := ParseTarget(rest, r.URL.RawQuery)
	if target == nil {
		// Only the token root is a homepage entry point. Any other
		// invalid target inside the token scope is a 404 (upstream
		// returns 404 for /default/https, /default/http/, ...).
		if rest == "" {
			s.serveHomepage(w, r)
			return
		}
		if s.cfg.DefaultPassword {
			s.redirectDefaultPassword(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}

	s.proxy.ServeHTTP(w, r, target)
}

// homepageAllowed reports whether this request may be served the homepage.
// With password protection enabled the homepage contains token-bearing links,
// so it is only available inside the token scope (e.g. /{token}/). The public
// root path is deliberately 404 after this fix.
func (s *Server) homepageAllowed(r *http.Request) bool {
	if s.cfg.noPassword() {
		return true
	}
	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	if bp := s.cfg.BasePath(); bp != "" {
		if path != bp && !strings.HasPrefix(path, bp+"/") {
			return false
		}
		path = strings.TrimPrefix(path, bp)
		if path == "" {
			path = "/"
		}
	}
	return strings.HasPrefix(path, s.cfg.TokenPrefix)
}

// targetFromPathToken implements the upstream mid-path token repair: an
// optional leading token prefix is stripped first, then the next occurrence
// of the token is used as an anchor for `{proto}/{host}/{path}`.
//
//	/foo/token/http/host/a       -> http://host/a
//	/token/foo/token/http/host/a -> http://host/a
func (s *Server) targetFromPathToken(path, rawQuery string) *TargetInfo {
	tokenPrefix := s.cfg.TokenPrefix
	if tokenPrefix == "" || tokenPrefix == "/" {
		return nil
	}
	path = strings.TrimPrefix(path, tokenPrefix)
	i := strings.Index(path, tokenPrefix)
	if i < 0 {
		return nil
	}
	rest := path[i+len(tokenPrefix):]
	// Upstream `_0x41151c` only rebuilds the target when something follows
	// the host (the trailing path is what proves this is a mangled link).
	// A bare `.../{token}/http/host` is left untouched and falls through.
	if m := pathPrefixRe.FindStringSubmatch(rest); m == nil || m[3] == "" {
		return nil
	}
	return ParseTarget(rest, rawQuery)
}

// repairOutOfScope mirrors the upstream server-side repair: when the request
// path is outside the token scope, the real site context is recovered from
// (1) the Referer header (must start with {proxy_url}{token_prefix}) or
// (2) the proxy_real_protocol / proxy_real_host control cookies, and the
// path is re-anchored under {token_prefix}{proto}/{host}.
func (s *Server) repairOutOfScope(r *http.Request) *TargetInfo {
	if s.cfg.TokenPrefix == "/" {
		return nil // every path is in scope
	}
	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if bp := s.cfg.BasePath(); bp != "" {
		if path != bp && !strings.HasPrefix(path, bp+"/") {
			return nil
		}
	}
	if bp := s.cfg.BasePath(); bp != "" && (path == bp || strings.HasPrefix(path, bp+"/")) {
		path = strings.TrimPrefix(path, bp)
		if path == "" {
			path = "/"
		}
	}
	// Never repair requests for the proxy's own static assets. /favicon.ico
	// is intentionally NOT here: a proxied page's favicon must be fetched
	// from its real site via the normal repair path.
	switch path {
	case "/siteproxy-response-injected.js", "/siteproxy_service_worker.js":
		return nil
	}

	var proto, host string
	prefix := s.cfg.ProxyURL + s.cfg.TokenPrefix
	if ref := r.Header.Get("Referer"); strings.HasPrefix(ref, prefix) {
		// Parse the full Referer instead of splitting the raw string: a page
		// at the target root with a query string (e.g. /token/http/host?x=1)
		// would otherwise put "host?x=1" into the host and fail validation.
		if t := targetFromProxiedURL(ref, s.cfg.TokenPrefix, s.cfg.BasePath()); t != nil {
			proto, host = t.Protocol, t.Host
		}
	}
	if proto == "" || host == "" {
		proto = ""
		host = ""
		for _, part := range strings.Split(r.Header.Get("Cookie"), ";") {
			part = strings.TrimSpace(part)
			if i := strings.IndexByte(part, '='); i > 0 {
				switch part[:i] {
				case "proxy_real_protocol":
					proto = part[i+1:]
				case "proxy_real_host":
					host = part[i+1:]
				}
			}
		}
	}
	if (proto != "http" && proto != "https") || host == "" {
		return nil
	}
	// The control cookies are client-visible (the injected runtime also
	// writes non-HttpOnly copies), so a malicious page could plant an
	// arbitrary proxy_real_host. Validate it exactly like ParseTarget
	// does before it may be used as an upstream host (no userinfo
	// smuggle, no whitespace, plausible hostname shape).
	normalizedHost, ok := normalizeTargetHost(host)
	if !ok {
		return nil
	}
	return &TargetInfo{Protocol: proto, Host: normalizedHost, Path: path, RawQuery: r.URL.RawQuery}
}

// isPlausibleHost validates a target host before it is used to build an
// upstream URL or a control-cookie repair. It accepts DNS names, IPv4,
// bracketed IPv6 literals and IDN labels (converted to punycode), with an
// optional numeric port. Short/intranet hostnames (localhost, router) remain
// valid targets.
func isPlausibleHost(host string) bool {
	_, ok := normalizeTargetHost(host)
	return ok
}

// normalizeTargetHost validates a target host and returns its canonical
// upstream form. Non-ASCII labels and percent-encoded UTF-8 labels (the shape
// a browser sends in a path) are converted to ASCII IDNA/punycode, so an
// internationalized domain does not turn into a routing 404.
func normalizeTargetHost(host string) (string, bool) {
	if host == "" {
		return "", false
	}
	if strings.Contains(host, "%") {
		decoded, err := url.PathUnescape(host)
		if err != nil {
			return "", false
		}
		host = decoded
	}
	if len(host) > 253 {
		return "", false
	}
	if strings.ContainsAny(host, "/\\?#@ \t\r\n\v\f\x00") {
		return "", false
	}
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return "", false
		}
		if net.ParseIP(host[1:end]) == nil {
			return "", false
		}
		rest := host[end+1:]
		if rest == "" {
			return host, true
		}
		if !strings.HasPrefix(rest, ":") {
			return "", false
		}
		if !validPort(rest[1:]) {
			return "", false
		}
		return host, true
	}
	name := host
	port := ""
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		if strings.Count(host, ":") != 1 {
			return "", false
		}
		name = host[:i]
		port = host[i+1:]
		if !validPort(port) {
			return "", false
		}
	}
	if name == "" {
		return "", false
	}
	ascii := name
	if !isASCIIHostName(name) {
		converted, err := idna.ToASCII(name)
		if err != nil || converted == "" {
			return "", false
		}
		ascii = converted
	}
	if !isValidASCIIHostName(ascii) {
		return "", false
	}
	if len(ascii) > 253 {
		return "", false
	}
	if port != "" {
		return ascii + ":" + port, true
	}
	return ascii, true
}

func isASCIIHostName(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func isValidASCIIHostName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '.' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func validPort(p string) bool {
	if p == "" {
		return false
	}
	n, err := strconv.Atoi(p)
	return err == nil && n > 0 && n <= 65535
}

// serveHomepage serves the ported netptop homepage, processed through the
// same HTML rewriter used for proxied pages so the links stay inside the
// proxy and the runtime scripts get injected.
func (s *Server) serveHomepage(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DefaultPassword {
		// Upstream `default_password` deployments never show the local
		// token-scoped index: / and /{token}/ redirect to the public entry.
		s.redirectDefaultPassword(w, r)
		return
	}
	if !s.homepageAllowed(r) {
		http.NotFound(w, r)
		return
	}
	inject := renderInlineBootstrap(s.cfg, "https", "") + injectedScriptTagAt(s.cfg.BasePath())
	html := rewriteHTML(string(s.homepageHTML), s.cfg.ProxyURL+s.cfg.TokenPrefix,
		s.cfg.ProxyURL+s.cfg.TokenPrefix, hostOfURL(s.cfg.ProxyURL), inject)
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Content-Length", fmt.Sprint(len(html)))
	_, _ = w.Write([]byte(html))
}

// redirectDefaultPassword mirrors upstream `default_password`: after
// token_prefix was normalised from "", "/" or "//", the proxy redirects its
// homepage and invalid paths to the public netptop entry (still through the
// proxy, so the user never leaves the configured public URL).
func (s *Server) redirectDefaultPassword(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, s.cfg.ProxyURL+s.cfg.TokenPrefix+"https/www.netptop.com", http.StatusFound)
}

// serveServiceWorker renders the service worker with the current site context.
// The registration URL carries proxy_real_protocol/proxy_real_host and the
// configured token_prefix. The token check is what keeps the worker (which
// necessarily contains the token for path-scope checks) from being fetched
// anonymously; a bare public request gets a 404 instead of the secret.
// Upstream also returns 404 when proxy_real_host is missing or invalid instead
// of falling back to a Referer, so a malformed registration cannot obtain a
// worker with an empty/attacker-selected target context.
func (s *Server) serveServiceWorker(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token_prefix")
	if token != s.cfg.TokenPrefix {
		http.NotFound(w, r)
		return
	}
	proto := r.URL.Query().Get("proxy_real_protocol")
	if proto != "http" && proto != "https" {
		proto = "https"
	}
	host, ok := normalizeTargetHost(r.URL.Query().Get("proxy_real_host"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, err := renderServiceWorker(s.cfg, proto, host, token)
	if err != nil {
		http.Error(w, "siteproxy: render service worker: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.serveStaticJS(w, r, body, s.swAllowedScope())
}

// serveInjectedScript serves the static client runtime. All per-page context
// comes from the inline bootstrap globals, so this unauthenticated endpoint
// contains neither token_prefix nor any other secret.
func (s *Server) serveInjectedScript(w http.ResponseWriter, r *http.Request) {
	s.serveStaticJS(w, r, assetsInjected, s.swAllowedScope())
}

// swAllowedScope returns the Service-Worker-Allowed value for a deployment:
// the base path plus a trailing slash ("" base -> "/").
func (s *Server) swAllowedScope() string {
	if bp := s.cfg.BasePath(); bp != "" {
		return bp + "/"
	}
	return "/"
}

// targetFromProxiedURL parses a proxied absolute URL back into a TargetInfo.
func targetFromProxiedURL(u, tokenPrefix, basePath string) *TargetInfo {
	path := pathOfProxied(u, tokenPrefix)
	if basePath != "" && (path == basePath || strings.HasPrefix(path, basePath+"/")) {
		path = strings.TrimPrefix(path, basePath)
		if path == "" {
			path = "/"
		}
	}
	if tokenPrefix != "/" {
		if !strings.HasPrefix(path, tokenPrefix) {
			return nil
		}
		path = path[len(tokenPrefix):]
	}
	return ParseTarget(path, "")
}

func (s *Server) serveStaticJS(w http.ResponseWriter, r *http.Request, body []byte, allowedScope string) {
	w.Header().Set("Content-Type", "application/javascript; charset=UTF-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Service-Worker-Allowed", allowedScope)
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	_, _ = w.Write(body)
}

// renderHomepage rewrites every absolute link of the embedded homepage into
// its proxied path form.
func renderHomepage(cfg *Config) []byte {
	return []byte(rewriteHomepageLinks(string(assetsHome), cfg, hostOfURL(cfg.ProxyURL)))
}

// rewriteHomepageLinks rewrites every href="https://..." anchor of the
// homepage into "{token_prefix}https/..." form.
func rewriteHomepageLinks(html string, cfg *Config, proxyHost string) string {
	const marker = `href="`
	var out strings.Builder
	out.Grow(len(html) + 512)
	s := html
	for {
		i := strings.Index(s, marker)
		if i < 0 {
			out.WriteString(s)
			break
		}
		out.WriteString(s[:i+len(marker)])
		s = s[i+len(marker):]
		j := strings.Index(s, `"`)
		if j < 0 {
			out.WriteString(s)
			break
		}
		link := s[:j]
		if strings.HasPrefix(link, "https://") || strings.HasPrefix(link, "http://") {
			out.WriteString(RewriteURL(link, cfg.ProxyURL+cfg.TokenPrefix, proxyHost))
		} else {
			out.WriteString(link)
		}
		s = s[j:]
	}
	return out.String()
}

// pathOfProxied extracts the full path of a proxied URL.
func pathOfProxied(u, tokenPrefix string) string {
	i := strings.Index(u, "://")
	if i < 0 {
		return u
	}
	rest := u[i+3:]
	j := strings.Index(rest, "/")
	if j < 0 {
		return "/"
	}
	return rest[j:]
}
