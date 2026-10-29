package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
)

// Server is the top level HTTP handler implementing the siteproxy routing:
//
//	/                                  -> homepage
//	/index.html                        -> homepage
//	/siteproxy-response-injected.js    -> client runtime script
//	/siteproxy_service_worker.js       -> client service worker
//	{token_prefix}{proto}/{host}/...   -> proxied target
//	anything else                      -> homepage (same as upstream siteproxy)
type Server struct {
	cfg    *Config
	proxy  *Proxy
	client *http.Client

	injectedTemplate []byte
	homepageHTML     []byte
}

// NewServer wires the routing table and prepares the embedded assets.
func NewServer(cfg *Config) (*Server, error) {
	client := newHTTPClient()
	p, err := NewProxy(cfg, client)
	if err != nil {
		return nil, err
	}

	return &Server{
		cfg:              cfg,
		proxy:            p,
		client:           client,
		injectedTemplate: assetsInjected,
		homepageHTML:     renderHomepage(cfg),
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
	case "/siteproxy-response-injected.js":
		// Rendered per-request: current site context comes from Referer.
		s.serveInjectedScript(w, r)
		return
	case "/siteproxy_service_worker.js":
		// Service worker must be served from the origin scope root and is
		// rendered per request with the page's site context (query params,
		// falling back to Referer), like the original server.
		s.serveServiceWorker(w, r)
		return
	case "/favicon.ico":
		http.NotFound(w, r)
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
	case "/siteproxy-response-injected.js":
		s.serveInjectedScript(w, r)
		return
	case "/siteproxy_service_worker.js":
		s.serveServiceWorker(w, r)
		return
	case "/favicon.ico":
		http.NotFound(w, r)
		return
	}

	// Token (password) gate.
	if !s.cfg.noPassword() && !strings.HasPrefix(proxyPath, s.cfg.TokenPrefix) {
		// Upstream behaviour: a path outside the token scope is still
		// proxied when the request can be attributed to a proxied site
		// via Referer or the control cookies (both require knowledge
		// of the proxy session). Otherwise fall back to the homepage
		// so token-less visits do not leak target info.
		if repaired := s.repairOutOfScope(r); repaired != nil {
			s.proxy.ServeHTTP(w, r, repaired)
			return
		}
		s.serveHomepage(w, r)
		return
	}

	rest := strings.TrimPrefix(proxyPath, s.cfg.TokenPrefix)
	target := ParseTarget(rest, r.URL.RawQuery)
	if target == nil {
		// Inside the token scope but not a valid target: homepage
		// fallback (same as upstream).
		s.serveHomepage(w, r)
		return
	}

	s.proxy.ServeHTTP(w, r, target)
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
	// Never repair requests for the proxy's own static assets.
	switch path {
	case "/siteproxy-response-injected.js", "/siteproxy_service_worker.js", "/favicon.ico":
		return nil
	}

	var proto, host string
	prefix := s.cfg.ProxyURL + s.cfg.TokenPrefix
	if ref := r.Header.Get("Referer"); strings.HasPrefix(ref, prefix) {
		parts := strings.Split(strings.TrimPrefix(ref, prefix), "/")
		if len(parts) >= 2 {
			proto, host = parts[0], parts[1]
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
	if !isPlausibleHost(host) {
		return nil
	}
	return &TargetInfo{Protocol: proto, Host: host, Path: path, RawQuery: r.URL.RawQuery}
}

// isPlausibleHost validates a target host before it is used to build an
// upstream URL or a control-cookie repair. It accepts DNS names, IPv4 and
// bracketed IPv6 literals with an optional numeric port. Short/intranet
// hostnames (localhost, router) remain valid targets.
func isPlausibleHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	if strings.ContainsAny(host, "/\\?#@ \t\r\n\v\f\x00") {
		return false
	}
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return false
		}
		if net.ParseIP(host[1:end]) == nil {
			return false
		}
		rest := host[end+1:]
		if rest == "" {
			return true
		}
		if !strings.HasPrefix(rest, ":") {
			return false
		}
		return validPort(rest[1:])
	}
	name := host
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		name = host[:i]
		if !validPort(host[i+1:]) {
			return false
		}
	}
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
	inject := renderInlineBootstrap(s.cfg, "https", "") + injectedScriptTagAt(s.cfg.BasePath())
	html := rewriteHTML(string(s.homepageHTML), s.cfg.ProxyURL+s.cfg.TokenPrefix,
		s.cfg.ProxyURL+s.cfg.TokenPrefix, hostOfURL(s.cfg.ProxyURL), inject)
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Content-Length", fmt.Sprint(len(html)))
	_, _ = w.Write([]byte(html))
}

// serveServiceWorker renders the service worker with the current site context.
// Registration carries proxy_real_protocol/proxy_real_host as query params;
// requests without them fall back to the page Referer so the worker still
// knows the real origin for out-of-scope request repair.
func (s *Server) serveServiceWorker(w http.ResponseWriter, r *http.Request) {
	proto := r.URL.Query().Get("proxy_real_protocol")
	host := r.URL.Query().Get("proxy_real_host")
	if proto != "http" && proto != "https" {
		proto = "https"
	}
	if host != "" && !isPlausibleHost(host) {
		host = ""
	}
	if host == "" {
		if ref := r.Header.Get("Referer"); ref != "" {
			if t := targetFromProxiedURL(ref, s.cfg.TokenPrefix, s.cfg.BasePath()); t != nil {
				proto, host = t.Protocol, t.Host
			}
		}
	}
	body, err := renderServiceWorker(s.cfg, proto, host)
	if err != nil {
		http.Error(w, "siteproxy: render service worker: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.serveStaticJS(w, r, body, s.swAllowedScope())
}

// serveInjectedScript renders siteproxy-response-injected.js with the site
// context of the requesting page (parsed from its Referer). The script also
// re-derives the context from location.pathname at runtime, so a missing
// Referer is not fatal.
func (s *Server) serveInjectedScript(w http.ResponseWriter, r *http.Request) {
	proto, host := "https", ""
	if ref := r.Header.Get("Referer"); ref != "" {
		if t := targetFromProxiedURL(ref, s.cfg.TokenPrefix, s.cfg.BasePath()); t != nil {
			proto, host = t.Protocol, t.Host
		}
	}
	body := renderInjectedScript(s.cfg, proto, host)
	s.serveStaticJS(w, r, body, s.swAllowedScope())
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
	w.Header().Set("Cache-Control", "no-cache")
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
