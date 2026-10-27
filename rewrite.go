package main

import (
	"regexp"
	"strconv"
	"strings"
)

// ----------------------------------------------------------------------------
// Server-side URL rewriting for HTML / CSS / JS payloads.
//
// Semantics verified against the actual node-mode v2.7.11 bundle by runtime
// differential testing (scripts/audit2_differential.mjs):
//   - URL attributes (quoted / single-quoted / unquoted) are rewritten;
//   - srcset / imagesrcset candidates are each rewritten (v2.7.7);
//   - url() inside style attributes and <style> blocks is rewritten;
//   - a comma inside a URL is not treated as a list separator (v2.7.10);
//   - integrity / crossorigin attributes are dropped from script/link tags
//     (rewritten bodies would fail subresource integrity);
//   - inline <script> bodies: comments/strings/regex literals are preserved,
//     while `location` and the other identifiers the original node mode
//     rewrites are routed through the injected wrappers;
//   - external JS files: the same identifier rewrites plus the
//     ___location bootstrap line;
//   - <meta http-equiv=refresh> urls ARE rewritten here (the upstream leaves
//     them relative; both resolve correctly, absolute is safer with <base>).
//
// URL rewriting never runs inside <script> bodies; those are extracted first
// and restored after the URL pipeline, matching upstream's script split.
// ----------------------------------------------------------------------------

var (
	htmlURLAttrs = []string{"href", "src", "action", "poster", "formaction",
		"cite", "manifest",
		"data-src", "data-href", "data-url", "data-background", "data-original",
		"dynsrc", "lowsrc"}

	attrNamesAlt = `(?:` + strings.Join(htmlURLAttrs, "|") + `)`
	// attr=value with double quotes, single quotes, or no quotes at all
	// (several large sites emit unquoted attributes, e.g. baidu.com).
	attrValueRe = attrValueRegex(attrNamesAlt)

	srcsetAttrRe = attrValueRegex(`(?:srcset|imagesrcset)`)

	styleAttrRe = attrValueRegex(`(?:style)`)

	// Inline event handlers and javascript: URLs contain JavaScript even
	// though they are attributes; the upstream node mode rewrites them
	// (its identifier pass runs over the whole document). Handle them
	// explicitly so they cannot navigate outside the proxy.
	eventAttrRe = attrValueRegex(`on(?:abort|afterprint|auxclick|beforeinput|beforematch|beforeprint|beforetoggle|beforeunload|blur|canplay|canplaythrough|change|click|close|compositionend|compositionstart|compositionupdate|contextmenu|copy|cut|dblclick|drag|dragend|dragenter|dragleave|dragover|dragstart|drop|durationchange|emptied|ended|error|focus|focusin|focusout|formdata|fullscreenchange|fullscreenerror|gotpointercapture|hashchange|input|invalid|keydown|keypress|keyup|languagechange|load|loadeddata|loadedmetadata|loadstart|lostpointercapture|message|messageerror|mousedown|mouseenter|mouseleave|mousemove|mouseout|mouseover|mouseup|offline|online|pagehide|pageshow|paste|pause|play|playing|pointercancel|pointerdown|pointerenter|pointerleave|pointermove|pointerout|pointerover|pointerrawupdate|pointerup|popstate|progress|ratechange|rejectionhandled|reset|resize|scroll|scrollend|search|securitypolicyviolation|seeked|seeking|select|selectionchange|selectstart|slotchange|stalled|storage|submit|suspend|timeupdate|toggle|touchcancel|touchend|touchmove|touchstart|transitioncancel|transitionend|transitionrun|transitionstart|unhandledrejection|unload|volumechange|waiting|wheel)`)
	jsURLAttrRe = attrValueRegex(`(?:href|src|action|formaction|data)`)

	// Any quoted URL inside a tag (not just the known URL attributes).
	// Upstream rewrites these through its generic quoted-URL pass; covering
	// them here fixes meta/OG/data-* attributes and object[data] without
	// touching visible text outside tags.
	tagRe          = regexp.MustCompile(`<(?:[^>"']|"[^"]*"|'[^']*')*>`)
	tagQuotedURLRe = regexp.MustCompile(`(?i)(["'])((?:https?://|//|/)[^"'\s]*)["']`)

	baseTagRe = regexp.MustCompile(`(?i)(<base[^>]*?href\s*=\s*)(")([^"]*)(")|(<base[^>]*?href\s*=\s*)(')([^']*)(')|(<base[^>]*?href\s*=\s*)([^\s"'>][^\s>]*)`)

	styleTagRe        = regexp.MustCompile(`(?is)(<style[^>]*>)(.*?)(</style\s*>)`)
	scriptTagRe       = regexp.MustCompile(`(?is)(<script[^>]*>)(.*?)(</script\s*>)`)
	scriptOrLinkTagRe = regexp.MustCompile(`(?is)<(?:script|link)\b[^>]*>`)

	integrityAttrRe = regexp.MustCompile(`(?i)\s(?:integrity|crossorigin)(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+))?`)

	metaRefreshContentRe = regexp.MustCompile(`(?i)(\d*\s*;\s*url\s*=\s*)(["']?)([^"'>\s]+)`)

	injectAfterHeadRe = regexp.MustCompile(`(?i)<head[^>]*>`)
	injectAfterBodyRe = regexp.MustCompile(`(?i)<body[^>]*>`)
	injectAfterHTMLRe = regexp.MustCompile(`(?i)<html[^>]*>`)
	doctypeRe         = regexp.MustCompile(`(?i)^\s*<!DOCTYPE[^>]*>`)
	// Fallback anchor used by the upstream server: the first closing tag.
	firstClosingTagRe = regexp.MustCompile(`</[a-zA-Z0-9]+>`)

	charsetMetaSnippet = `<meta http-equiv="Content-Type" content="text/html; charset=UTF-8">`
)

// injectedScriptTag is the external client runtime script tag (upstream v=4).
const injectedScriptTag = `<script src="/siteproxy-response-injected.js?v=4"></script>`

// attrValueRegex builds a three-branch attribute-value matcher (double
// quoted, single quoted, unquoted). Group layout:
//
//	double-quoted:  1=prefix 2=" 3=value 4="
//	single-quoted:  5=prefix 6=' 7=value 8='
//	unquoted:       9=prefix 10=value
func attrValueRegex(names string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(\s(?:` + names + `)\s*=\s*)(")([^"]*)(")|(\s(?:` + names + `)\s*=\s*)(')([^']*)(')|(\s(?:` + names + `)\s*=\s*)([^\s"'>][^\s>]*)`)
}

// rewriteHTML performs full server-side rewriting of an HTML document.
//
//	publicPrefix is "{proxy_url}{token_prefix}"; baseProxiedURL is the fully
//	proxied URL of the current document and is used to resolve relative
//	references (same semantics as <base href>). injectSnippet is the fully
//	rendered injection block (charset meta + inline bootstrap + external
//	runtime tag) — it is page-specific because the inline bootstrap carries
//	the current site context.
func rewriteHTML(html, publicPrefix, baseProxiedURL, proxyHost, injectSnippet string) string {
	// Script bodies must not be touched by the URL rewriter (a regex
	// literal or a string containing `href="..."` would be corrupted).
	// Stash them first and restore after the URL pipeline.
	html, scriptBodies := extractScriptBodies(html)

	// The output is always UTF-8, so normalise any legacy charset meta
	// while script bodies are still stashed (never touch JS strings).
	html = rewriteCharsetMeta(html)

	// 0. Quoted absolute/root-relative URLs in arbitrary tag attributes
	//    (upstream generic quoted-URL pass). Script bodies are already
	//    stashed, so only real attributes are affected.
	html = rewriteTagQuotedURLs(html, publicPrefix, baseProxiedURL, proxyHost)

	// 1. Plain URL attributes.
	html = rewriteQuotedAttrs(html, attrValueRe, func(v string) string {
		return RewriteWithBase(v, publicPrefix, baseProxiedURL, proxyHost)
	})

	// 1b. JavaScript embedded in event handlers / javascript: URLs.
	html = rewriteQuotedAttrs(html, jsURLAttrRe, rewriteJavascriptURLValue)
	html = rewriteQuotedAttrs(html, eventAttrRe, rewriteEventAttr)

	// 2. srcset / imagesrcset candidate lists.
	html = rewriteQuotedAttrs(html, srcsetAttrRe, func(v string) string {
		return rewriteSrcset(v, publicPrefix, baseProxiedURL, proxyHost)
	})

	// 3. style="...url(...)" attributes (v2.7.9) and <style> blocks.
	html = rewriteQuotedAttrs(html, styleAttrRe, func(v string) string {
		return rewriteCSS(v, publicPrefix, baseProxiedURL, proxyHost)
	})
	html = styleTagRe.ReplaceAllStringFunc(html, func(m string) string {
		g := styleTagRe.FindStringSubmatch(m)
		return g[1] + rewriteCSS(g[2], publicPrefix, baseProxiedURL, proxyHost) + g[3]
	})

	// 4. <base href> rewrite (kept, so that browser-side relative
	//    resolution continues to work inside the proxy scope).
	html = rewriteQuotedAttrs(html, baseTagRe, func(v string) string {
		return RewriteWithBase(v, publicPrefix, baseProxiedURL, proxyHost)
	})

	// 5. <meta http-equiv=refresh content="2; url=...">.
	html = rewriteMetaRefresh(html, publicPrefix, baseProxiedURL, proxyHost)

	// 6. Drop integrity/crossorigin from script/link tags only (rewritten
	//    bodies break the hashes; removing them from inline JS strings
	//    would corrupt the script).
	html = scriptOrLinkTagRe.ReplaceAllStringFunc(html, func(tag string) string {
		return integrityAttrRe.ReplaceAllString(tag, "")
	})

	// 7. Restore inline scripts, routing location/navigator identifiers
	//    through the injected wrappers.
	html = restoreScriptBodies(html, scriptBodies)

	return injectScripts(html, injectSnippet)
}

const scriptBodyMarkerPrefix = "@@SITEPROXY_SCRIPT_BODY_"

func scriptBodyMarker(i int) string {
	return scriptBodyMarkerPrefix + strconv.Itoa(i) + "@@"
}

// extractScriptBodies replaces every <script> body with a unique marker so the
// URL rewriter can run over the document without touching JavaScript.
func extractScriptBodies(html string) (string, []string) {
	var bodies []string
	out := scriptTagRe.ReplaceAllStringFunc(html, func(m string) string {
		g := scriptTagRe.FindStringSubmatch(m)
		bodies = append(bodies, g[2])
		return g[1] + scriptBodyMarker(len(bodies)-1) + g[3]
	})
	return out, bodies
}

func restoreScriptBodies(html string, bodies []string) string {
	for i, body := range bodies {
		html = strings.Replace(html, scriptBodyMarker(i), rewriteJSLocation(body), 1)
	}
	return html
}

// rewriteQuotedAttrs applies fn to each captured attribute value.
func rewriteQuotedAttrs(html string, re *regexp.Regexp, fn func(string) string) string {
	return re.ReplaceAllStringFunc(html, func(m string) string {
		g := re.FindStringSubmatch(m)
		// Double-quoted: g[1..4]; single-quoted: g[5..8]; unquoted: g[9..10].
		if g[1] != "" {
			return g[1] + g[2] + fn(g[3]) + g[4]
		}
		if g[5] != "" {
			return g[5] + g[6] + fn(g[7]) + g[8]
		}
		return g[9] + fn(g[10])
	})
}

// rewriteTagQuotedURLs rewrites quoted URLs that appear inside HTML tags.
// The tag matcher keeps quoted attribute values intact, so URLs containing
// `>` or `<` are not truncated.
func rewriteTagQuotedURLs(html, publicPrefix, baseProxiedURL, proxyHost string) string {
	return tagRe.ReplaceAllStringFunc(html, func(tag string) string {
		return tagQuotedURLRe.ReplaceAllStringFunc(tag, func(m string) string {
			g := tagQuotedURLRe.FindStringSubmatch(m)
			return g[1] + RewriteWithBase(g[2], publicPrefix, baseProxiedURL, proxyHost) + g[1]
		})
	})
}

// rewriteJavascriptURLValue rewrites identifiers in a javascript: URL while
// preserving the scheme and any leading whitespace.
func rewriteJavascriptURLValue(v string) string {
	const scheme = "javascript:"
	trimmed := strings.TrimLeft(v, " \t\r\n\f\v")
	if len(trimmed) < len(scheme) || !strings.EqualFold(trimmed[:len(scheme)], scheme) {
		return v
	}
	js := v[len(v)-len(trimmed)+len(scheme):]
	if containsEntityQuote(js) {
		return v
	}
	return v[:len(v)-len(trimmed)+len(scheme)] + rewriteJSLocation(js)
}

// rewriteEventAttr rewrites identifiers in an inline handler. Handlers that
// use HTML-entity quotes (&quot; etc.) cannot be tokenised reliably as JS,
// so they are left untouched rather than corrupting string literals.
func rewriteEventAttr(v string) string {
	if containsEntityQuote(v) {
		return v
	}
	return rewriteJSLocation(v)
}

func containsEntityQuote(s string) bool {
	lower := strings.ToLower(s)
	for _, q := range cssEntityQuotes {
		if strings.Contains(lower, q) {
			return true
		}
	}
	return false
}

// rewriteMetaRefresh rewrites the url= part of meta refresh tags.
func rewriteMetaRefresh(html, publicPrefix, baseProxiedURL, proxyHost string) string {
	return regexp.MustCompile(`(?i)<meta\s[^>]*>`).ReplaceAllStringFunc(html, func(m string) string {
		lower := strings.ToLower(m)
		if !strings.Contains(lower, "refresh") || !strings.Contains(lower, "http-equiv") {
			return m
		}
		return metaRefreshContentRe.ReplaceAllStringFunc(m, func(cm string) string {
			g := metaRefreshContentRe.FindStringSubmatch(cm)
			prefix, quote, u := g[1], g[2], g[3]
			return prefix + quote + RewriteWithBase(u, publicPrefix, baseProxiedURL, proxyHost) + quote
		})
	})
}

// rewriteSrcset rewrites every candidate of a srcset/imagesrcset attribute
// using the ORIGINAL v2.7.10 parser (`_0x4898df`): the URL token runs until
// the first whitespace (so commas stay part of the URL), trailing commas are
// peeled off and re-emitted, and the descriptor runs until the next comma.
// This handles minified lists like "a.png 1x,b.png 2x" as well as URLs that
// contain commas.
func rewriteSrcset(v, publicPrefix, baseProxiedURL, proxyHost string) string {
	rewrite := func(u string) string {
		return RewriteWithBase(u, publicPrefix, baseProxiedURL, proxyHost)
	}
	isWS := func(c byte) bool {
		return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
	}
	var out strings.Builder
	i, n := 0, len(v)
	for i < n {
		// 1. Verbatim run of whitespace and commas.
		start := i
		for i < n && (isWS(v[i]) || v[i] == ',') {
			i++
		}
		out.WriteString(v[start:i])
		if i >= n {
			break
		}
		// 2. URL token: everything up to the next whitespace (commas inside).
		urlStart := i
		for i < n && !isWS(v[i]) {
			i++
		}
		url := v[urlStart:i]
		// 3. Peel trailing commas; they terminate the candidate.
		commas := 0
		for len(url) > 0 && url[len(url)-1] == ',' {
			url = url[:len(url)-1]
			commas++
		}
		out.WriteString(rewrite(url))
		out.WriteString(v[i-commas : i])
		if commas > 0 {
			continue // comma-terminated: no descriptor follows
		}
		// 4. Descriptor: verbatim until the next comma.
		descStart := i
		for i < n && v[i] != ',' {
			i++
		}
		out.WriteString(v[descStart:i])
	}
	return out.String()
}

// ----------------------------------------------------------------------------
// CSS rewriting: url(...) and @import "..."/'...'
// ----------------------------------------------------------------------------

var (
	cssURLRe    = regexp.MustCompile(`(?i)(url)\(\s*([^)]*?)\s*\)`)
	cssImportRe = regexp.MustCompile(`(?i)(@import\s+)(['"])([^'"]+)(['"])`)

	// Quoted URLs that are not inside url(...), e.g. image-set("... 1x").
	// Upstream's generic quoted-URL pass covers these as well.
	cssQuotedURLRe = regexp.MustCompile(`(?i)(["'])((?:https?://|//|/)[^"'\s]*)["']`)

	// HTML entities used as quotes inside CSS url(...) by some sites
	// (the upstream `_0x205d99` handles these explicitly).
	cssEntityQuotes = []string{"&quot;", "&#34;", "&apos;", "&#39;"}
)

// splitCSSURLQuotes separates the optional quoting from a CSS url() body.
// It understands both normal quotes and the HTML entities some sites emit
// (&quot;, &#34;, &apos;, &#39;). ok is false for malformed / ambiguous
// values, which are then left untouched.
func splitCSSURLQuotes(inner string) (quote, u string, ok bool) {
	if inner == "" {
		return "", "", true
	}
	if strings.ContainsAny(inner, "\"'") {
		if len(inner) < 2 || inner[len(inner)-1] != inner[0] {
			return "", "", false
		}
		if inner[0] != '"' && inner[0] != '\'' {
			return "", "", false
		}
		u = inner[1 : len(inner)-1]
		lower := strings.ToLower(u)
		for _, q := range cssEntityQuotes {
			if strings.HasPrefix(lower, q) {
				// e.g. url('&quot;/x&quot;'): leave it alone, matching the
				// upstream behaviour for this ambiguous shape.
				return "", "", false
			}
		}
		return inner[:1], u, true
	}
	lower := strings.ToLower(inner)
	for _, q := range cssEntityQuotes {
		if strings.HasPrefix(lower, q) {
			if len(inner) <= 2*len(q) || !strings.HasSuffix(lower, q) {
				return "", "", false
			}
			return inner[:len(q)], inner[len(q) : len(inner)-len(q)], true
		}
		if strings.HasSuffix(lower, q) {
			return "", "", false
		}
	}
	if strings.ContainsAny(inner, " \t\n\r\f") {
		return "", "", false
	}
	return "", inner, true
}

func rewriteCSS(css, publicPrefix, baseProxiedURL, proxyHost string) string {
	// Skip url(data:...) / url(blob:...) etc. (v2.7.2).
	css = cssURLRe.ReplaceAllStringFunc(css, func(m string) string {
		g := cssURLRe.FindStringSubmatch(m)
		fn, inner := g[1], strings.TrimSpace(g[2])
		quote, u, ok := splitCSSURLQuotes(inner)
		if !ok {
			return m
		}
		lower := strings.ToLower(u)
		if strings.HasPrefix(lower, "data:") || strings.HasPrefix(lower, "blob:") ||
			strings.HasPrefix(lower, "#") {
			return m
		}
		return fn + "(" + quote + RewriteWithBase(u, publicPrefix, baseProxiedURL, proxyHost) + quote + ")"
	})
	css = cssImportRe.ReplaceAllStringFunc(css, func(m string) string {
		g := cssImportRe.FindStringSubmatch(m)
		return g[1] + g[2] + RewriteWithBase(g[3], publicPrefix, baseProxiedURL, proxyHost) + g[4]
	})
	css = cssQuotedURLRe.ReplaceAllStringFunc(css, func(m string) string {
		g := cssQuotedURLRe.FindStringSubmatch(m)
		return g[1] + RewriteWithBase(g[2], publicPrefix, baseProxiedURL, proxyHost) + g[1]
	})
	return css
}

// ----------------------------------------------------------------------------
// JavaScript rewriting.
// ----------------------------------------------------------------------------

// rewriteJSLocation rewrites identifiers and legacy window.location
// assignments inside inline scripts (whitespace before '=' is preserved).
func rewriteJSLocation(js string) string {
	return rewriteJSIdentifiers(js)
}

// jsBootstrapLine is prepended to every proxied JavaScript file, exactly like
// the original siteproxy does: worker / global scopes get a ___location alias
// so scripts that reference it before the page runtime loads keep working.
const jsBootstrapLine = "if(typeof self!=='undefined'&&typeof self.___location==='undefined'){try{self.___location=self.location}catch(e){}}\n"

// rewriteJSFile mirrors the upstream node pipeline for external JavaScript
// files. The identifier scanner rewrites every `location` reference outside
// strings/comments/regex literals, including assignment/comparison forms.
func rewriteJSFile(js string) string {
	return rewriteJSIdentifiers(js)
}

// ----------------------------------------------------------------------------
// Script injection (same anchor priority as the original:
// <head> -> <html> -> <body> -> first closing tag -> DOCTYPE -> start).
// ----------------------------------------------------------------------------

// injectScripts prepends the UTF-8 charset meta and appends the injected
// <script> after the first anchor found. The meta is placed first so the
// browser always decodes the (transcoded) body as UTF-8. Unlike the upstream
// (which gives up when no anchor exists) we still inject at the DOCTYPE or
// document start so fragments keep working.
func injectScripts(html, injectSnippet string) string {
	inject := charsetMetaSnippet + injectSnippet
	if loc := injectAfterHeadRe.FindStringIndex(html); loc != nil {
		return html[:loc[1]] + inject + html[loc[1]:]
	}
	if loc := injectAfterHTMLRe.FindStringIndex(html); loc != nil {
		return html[:loc[1]] + inject + html[loc[1]:]
	}
	if loc := injectAfterBodyRe.FindStringIndex(html); loc != nil {
		return html[:loc[1]] + inject + html[loc[1]:]
	}
	if loc := firstClosingTagRe.FindStringIndex(html); loc != nil {
		return html[:loc[1]] + inject + html[loc[1]:]
	}
	if m := doctypeRe.FindStringIndex(html); m != nil {
		return html[:m[1]] + inject + html[m[1]:]
	}
	return inject + html
}
