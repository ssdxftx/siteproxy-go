package main

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed assets
var assetsFS embed.FS

var (
	assetsHome     = mustRead("assets/index.html")
	assetsInjected = mustRead("assets/injected.tmpl.js")
	assetsInline   = mustRead("assets/injected-inline.tmpl.js")
	assetsWorker   = mustRead("assets/service-worker.tmpl.js")
)

func mustRead(name string) []byte {
	data, err := assetsFS.ReadFile(name)
	if err != nil {
		panic("siteproxy: missing embedded asset " + name + ": " + err.Error())
	}
	return data
}

// renderInjectedScript bakes the config and the current site context into the
// client runtime script (same technique as the original: the values are
// inlined as JS string constants).
func renderInjectedScript(cfg *Config, proto, host string) []byte {
	body := string(assetsInjected)
	body = strings.ReplaceAll(body, "__SITEPROXY_CONFIG_JSON__", jsObject(cfg, proto, host))
	return []byte(body)
}

// renderInlineBootstrap reproduces the inline <script> block the original
// node server injects before the external runtime (verbatim upstream text
// with the config values baked in).
func renderInlineBootstrap(cfg *Config, proto, host string) string {
	s := string(assetsInline)
	s = strings.ReplaceAll(s, "__PP__", jsEscape(cfg.ProxyURL+cfg.TokenPrefix))
	s = strings.ReplaceAll(s, "__PROTO__", jsEscape(proto))
	s = strings.ReplaceAll(s, "__HOST__", jsEscape(host))
	s = strings.ReplaceAll(s, "__CURL__", jsEscape(cfg.ProxyURL))
	s = strings.ReplaceAll(s, "__TOKEN__", jsEscape(cfg.TokenPrefix))
	return s
}

// renderServiceWorker bakes the token prefix into the service worker script.
func renderServiceWorker(cfg *Config) ([]byte, error) {
	body := string(assetsWorker)
	body = strings.ReplaceAll(body, "__SITEPROXY_CONFIG_JSON__", jsObject(cfg, "https", ""))
	return []byte(body), nil
}

// jsObject serialises the runtime config as a JS object literal.
func jsObject(cfg *Config, proto, host string) string {
	proto = jsEscape(proto)
	host = jsEscape(host)
	return fmt.Sprintf(`{
  proxy_url: %q,
  token_prefix: %q,
  proxy_real_protocol: %q,
  proxy_real_host: %q
}`, cfg.ProxyURL, cfg.TokenPrefix, proto, host)
}

// jsEscape guards against </script> breakouts inside inline constants.
func jsEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "'", `\'`)
	s = strings.ReplaceAll(s, "</", `<\/`)
	return s
}
