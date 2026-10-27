package main

import (
	"embed"
	"encoding/json"
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

// renderServiceWorker bakes the config and the current site context into the
// service worker script. The context is supplied by the registration URL
// (proxy_real_protocol/proxy_real_host) or derived from the Referer.
func renderServiceWorker(cfg *Config, proto, host string) ([]byte, error) {
	body := string(assetsWorker)
	body = strings.ReplaceAll(body, "__SITEPROXY_CONFIG_JSON__", jsObject(cfg, proto, host))
	return []byte(body), nil
}

// jsObject serialises the runtime config as a JS object literal.
func jsObject(cfg *Config, proto, host string) string {
	return fmt.Sprintf(`{
  proxy_url: %s,
  token_prefix: %s,
  proxy_real_protocol: %s,
  proxy_real_host: %s
}`, jsString(cfg.ProxyURL), jsString(cfg.TokenPrefix), jsString(proto), jsString(host))
}

// jsString serialises a value as a JavaScript/JSON string literal. JSON
// escaping also covers quotes, backslashes, control characters and
// U+2028/U+2029, which are line terminators in older JS engines.
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// jsEscape guards against </script> breakouts inside inline constants.
func jsEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "'", `\'`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\u2028", `\u2028`)
	s = strings.ReplaceAll(s, "\u2029", `\u2029`)
	s = strings.ReplaceAll(s, "</", `<\/`)
	return s
}
