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

// The external runtime is served as a static asset and reads its per-page
// config from the globals defined by the inline bootstrap. This keeps the
// (secret) token_prefix out of an unauthenticated, directly fetchable endpoint.
// Do not reintroduce config baking here.

// renderInlineBootstrap reproduces the inline <script> block the original
// node server injects before the external runtime (verbatim upstream text
// with the config values baked in).
func renderInlineBootstrap(cfg *Config, proto, host string) string {
	// Single pass: a value that happens to contain another placeholder must
	// not be substituted a second time.
	return strings.NewReplacer(
		"__PP__", jsEscape(cfg.ProxyURL+cfg.TokenPrefix),
		"__PROTO__", jsEscape(proto),
		"__HOST__", jsEscape(host),
		"__CURL__", jsEscape(cfg.ProxyURL),
		"__TOKEN__", jsEscape(cfg.TokenPrefix),
	).Replace(string(assetsInline))
}

// renderServiceWorker bakes the config and the current site context into the
// service worker script. The context (and the token, when the request is
// authenticated by the registration query) is supplied by the registration
// URL; a bare public fetch without a valid token gets an empty token and
// cannot be used as an authenticated worker.
func renderServiceWorker(cfg *Config, proto, host, token string) ([]byte, error) {
	body := string(assetsWorker)
	body = strings.ReplaceAll(body, "__SITEPROXY_CONFIG_JSON__", jsObject(cfg, proto, host, token))
	return []byte(body), nil
}

// jsObject serialises the runtime config as a JS object literal. tokenOverride
// lets the service-worker endpoint use the registration token (or "" when the
// request is not authenticated) instead of the configured secret.
func jsObject(cfg *Config, proto, host string, tokenOverride ...string) string {
	token := cfg.TokenPrefix
	if len(tokenOverride) > 0 {
		token = tokenOverride[0]
	}
	return fmt.Sprintf(`{
  proxy_url: %s,
  token_prefix: %s,
  proxy_real_protocol: %s,
  proxy_real_host: %s
}`, jsString(cfg.ProxyURL), jsString(token), jsString(proto), jsString(host))
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
