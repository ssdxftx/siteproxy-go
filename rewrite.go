package main

import (
        "regexp"
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
//   - integrity / crossorigin attributes are dropped (rewritten bodies would
//     fail subresource integrity);
//   - inline <script> bodies: window.location / window.location.href
//     assignments are routed through the injected window.___location wrapper
//     (v2.7.0), everything else stays byte-identical;
//   - external JS files: the same two assignment rewrites with upstream's
//     whitespace-eating regex shape, plus the ___location bootstrap line;
//   - <meta http-equiv=refresh> urls ARE rewritten here (the upstream leaves
//     them relative; both resolve correctly, absolute is safer with <base>).
//
// NOTE: the identifier rewrites and the whole-document quoted-URL pass that
// exist in the bundle only run in the Cloudflare-worker mode, not in the
// node mode this port mirrors — they are intentionally NOT implemented.
// ----------------------------------------------------------------------------

var (
        htmlURLAttrs = []string{"href", "src", "action", "poster", "formaction",
                "data-src", "data-href", "data-url", "data-background", "data-original",
                "dynsrc", "lowsrc", "ping"}

        attrNamesAlt = `(?:` + strings.Join(htmlURLAttrs, "|") + `)`
        // attr=value with double quotes, single quotes, or no quotes at all
        // (several large sites emit unquoted attributes, e.g. baidu.com).
        attrValueRe = attrValueRegex(attrNamesAlt)

        srcsetAttrRe = attrValueRegex(`(?:srcset|imagesrcset)`)

        styleAttrRe = attrValueRegex(`(?:style)`)

        baseTagRe = regexp.MustCompile(`(<base[^>]*?href\s*=\s*)(")([^"]*)(")|(<base[^>]*?href\s*=\s*)(')([^']*)(')|(<base[^>]*?href\s*=\s*)([^\s"'>][^\s>]*)`)

        styleTagRe  = regexp.MustCompile(`(?is)(<style[^>]*>)(.*?)(</style\s*>)`)
        scriptTagRe = regexp.MustCompile(`(?is)(<script[^>]*>)(.*?)(</script\s*>)`)

        integrityAttrRe = regexp.MustCompile(`(?i)\s(?:integrity|crossorigin)(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+))?`)

        metaRefreshContentRe = regexp.MustCompile(`(?i)(\d*\s*;\s*url\s*=\s*)(["']?)([^"'>\s]+)`)

        injectAfterHeadRe = regexp.MustCompile(`(?i)<head[^>]*>`)
        injectAfterBodyRe = regexp.MustCompile(`(?i)<body[^>]*>`)
        injectAfterHTMLRe = regexp.MustCompile(`(?i)<html[^>]*>`)
        doctypeRe         = regexp.MustCompile(`(?i)^\s*<!DOCTYPE[^>]*>`)
        // Fallback anchor used by the upstream server: the first closing tag.
        firstClosingTagRe = regexp.MustCompile(`</[a-zA-Z0-9]+>`)

        // window.location assignment rewrites (v2.7.0). RE2 has no lookahead, so
        // "=X" consumes one non-'=' char which the replacement keeps; "=="/"==="
        // comparisons can never match. (Inline-script shape: whitespace kept.)
        winLocHrefAssignRe = regexp.MustCompile(`window\.location\.href(\s*=[^=])`)
        winLocAssignRe     = regexp.MustCompile(`window\.location(\s*=[^=])`)

        // External JS file shape (upstream `_0x33f3fc`): the whitespace before
        // '=' is consumed by `\s*=` and the lazy group captures nothing.
        jsFileLocHrefAssignRe = regexp.MustCompile(`\bwindow\.location\.href\s*=(.*?)`)
        jsFileLocAssignRe     = regexp.MustCompile(`\bwindow\.location\s*=(.*?)`)

        charsetMetaSnippet = `<meta http-equiv="Content-Type" content="text/html; charset=UTF-8">`
)

// injectedScriptTag is the external client runtime script tag (upstream v=4).
const injectedScriptTag = `<script src="/siteproxy-response-injected.js?v=4"></script>`

// attrValueRegex builds a three-branch attribute-value matcher (double
// quoted, single quoted, unquoted). Group layout:
//
//      double-quoted:  1=prefix 2=" 3=value 4="
//      single-quoted:  5=prefix 6=' 7=value 8='
//      unquoted:       9=prefix 10=value
func attrValueRegex(names string) *regexp.Regexp {
        return regexp.MustCompile(`(\s(?:` + names + `)\s*=\s*)(")([^"]*)(")|(\s(?:` + names + `)\s*=\s*)(')([^']*)(')|(\s(?:` + names + `)\s*=\s*)([^\s"'>][^\s>]*)`)
}

// rewriteHTML performs full server-side rewriting of an HTML document.
//
//      publicPrefix is "{proxy_url}{token_prefix}"; baseProxiedURL is the fully
//      proxied URL of the current document and is used to resolve relative
//      references (same semantics as <base href>). injectSnippet is the fully
//      rendered injection block (charset meta + inline bootstrap + external
//      runtime tag) — it is page-specific because the inline bootstrap carries
//      the current site context.
func rewriteHTML(html, publicPrefix, baseProxiedURL, proxyHost, injectSnippet string) string {
        // 1. Plain URL attributes.
        html = rewriteQuotedAttrs(html, attrValueRe, func(v string) string {
                return RewriteWithBase(v, publicPrefix, baseProxiedURL, proxyHost)
        })

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

        // 6. Drop integrity/crossorigin (rewritten bodies break the hashes).
        html = integrityAttrRe.ReplaceAllString(html, "")

        // 7. Inline <script> blocks: route window.location writes through the
        //    injected wrapper; bodies are otherwise byte-identical (upstream).
        html = scriptTagRe.ReplaceAllStringFunc(html, func(m string) string {
                g := scriptTagRe.FindStringSubmatch(m)
                return g[1] + rewriteJSLocation(g[2]) + g[3]
        })

        return injectScripts(html, injectSnippet)
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
        cssURLRe    = regexp.MustCompile(`(?i)url\(\s*(['"]?)([^)'"]+)(['"]?)\s*\)`)
        cssImportRe = regexp.MustCompile(`(?i)(@import\s+)(['"])([^'"]+)(['"])`)
)

func rewriteCSS(css, publicPrefix, baseProxiedURL, proxyHost string) string {
        // Skip url(data:...) / url(blob:...) etc. (v2.7.2).
        css = cssURLRe.ReplaceAllStringFunc(css, func(m string) string {
                g := cssURLRe.FindStringSubmatch(m)
                quote, u := g[1], g[2]
                lower := strings.ToLower(u)
                if strings.HasPrefix(lower, "data:") || strings.HasPrefix(lower, "blob:") ||
                        strings.HasPrefix(lower, "#") {
                        return m
                }
                return "url(" + quote + RewriteWithBase(u, publicPrefix, baseProxiedURL, proxyHost) + quote + ")"
        })
        css = cssImportRe.ReplaceAllStringFunc(css, func(m string) string {
                g := cssImportRe.FindStringSubmatch(m)
                return g[1] + g[2] + RewriteWithBase(g[3], publicPrefix, baseProxiedURL, proxyHost) + g[4]
        })
        return css
}

// ----------------------------------------------------------------------------
// JavaScript rewriting.
// ----------------------------------------------------------------------------

// rewriteJSLocation routes window.location assignments through the wrapper
// (inline-script shape: whitespace before '=' is preserved).
func rewriteJSLocation(js string) string {
        js = winLocHrefAssignRe.ReplaceAllString(js, "window.___location.href$1")
        js = winLocAssignRe.ReplaceAllString(js, "window.___location$1")
        return js
}

// jsBootstrapLine is prepended to every proxied JavaScript file, exactly like
// the original siteproxy does: worker / global scopes get a ___location alias
// so scripts that reference it before the page runtime loads keep working.
const jsBootstrapLine = "if(typeof self!=='undefined'&&typeof self.___location==='undefined'){try{self.___location=self.location}catch(e){}}\n"

// rewriteJSFile mirrors the upstream node pipeline for external JavaScript
// files: only the two window.location assignment forms are rewritten, with
// the upstream whitespace-eating regex shape (`\s*=` consumes the blank).
// All other content — including bare `location` identifiers and string
// literals — stays untouched (verified by differential testing).
func rewriteJSFile(js string) string {
        js = jsFileLocHrefAssignRe.ReplaceAllString(js, "window.___location.href=$1")
        js = jsFileLocAssignRe.ReplaceAllString(js, "window.___location=$1")
        return js
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
