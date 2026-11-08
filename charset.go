package main

import (
	"bytes"
	"regexp"
	"strings"

	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/unicode"
)

// ----------------------------------------------------------------------------
// Charset handling (v2.5.0 "fixed ... gbk etc encoding issue").
//
// Upstream bodies may be GBK / GB18030 / Big5 / Shift-JIS / EUC-KR /
// ISO-8859-x. We transcode everything to UTF-8 and inject a leading UTF-8
// Content-Type meta so the browser renders it correctly.
// ----------------------------------------------------------------------------

var (
	xmlEncodingRe     = regexp.MustCompile(`(?is)<\?xml[^>]+encoding\s*=\s*["']([a-z0-9_\-]+)`)
	bomUTF16LE        = []byte{0xFF, 0xFE}
	bomUTF16BE        = []byte{0xFE, 0xFF}
	bomUTF8           = []byte{0xEF, 0xBB, 0xBF}
	sniffWindowBytes  = 64 << 10
	supportedCharset  = regexp.MustCompile(`(?i)^(gb2312|gbk|gb18030|big5|big5-hkscs|shift[_-]?jis|sjis|euc-?jp|euc-?kr|iso-?8859-[0-9]+|windows-12[0-9][0-9]|koi8-[ru]|latin[1-9])$`)
	utf16CharsetNames = regexp.MustCompile(`(?i)^utf-?16(le|be|)?$`)
	cssCharsetRe      = regexp.MustCompile(`(?i)^\s*@charset\s+["']([^"']+)["']\s*;`)
	charsetParamRe    = regexp.MustCompile(`(?i)^charset\s*=\s*(.+)$`)

	metaTagRe = regexp.MustCompile(`(?is)<meta(?:[^>"']|"[^"]*"|'[^']*')*>`)
)

func isHTMLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f'
}

// metaAttrValue parses one attribute of a <meta> tag. Walking the tag
// attribute-by-attribute (instead of regex-matching "charset=" anywhere)
// keeps a description/OG attribute whose value merely mentions
// "charset=gbk" from being mistaken for a real charset declaration.
func metaAttrValue(tag, want string) (string, bool) {
	i := len("<meta")
	n := len(tag)
	for i < n {
		for i < n && isHTMLSpace(tag[i]) {
			i++
		}
		if i >= n || tag[i] == '>' || tag[i] == '/' {
			break
		}
		start := i
		for i < n && !isHTMLSpace(tag[i]) && tag[i] != '=' && tag[i] != '>' {
			i++
		}
		name := strings.ToLower(tag[start:i])
		for i < n && isHTMLSpace(tag[i]) {
			i++
		}
		val := ""
		if i < n && tag[i] == '=' {
			i++
			for i < n && isHTMLSpace(tag[i]) {
				i++
			}
			if i < n && (tag[i] == '"' || tag[i] == '\'') {
				q := tag[i]
				i++
				vs := i
				for i < n && tag[i] != q {
					i++
				}
				val = tag[vs:i]
				if i < n {
					i++
				}
			} else {
				vs := i
				for i < n && !isHTMLSpace(tag[i]) && tag[i] != '>' {
					i++
				}
				val = tag[vs:i]
			}
		}
		if name == want {
			return strings.TrimSpace(val), true
		}
	}
	return "", false
}

// metaAttrSpan returns the value span of an attribute (inside its quotes for
// quoted values), so a charset declaration can be updated without touching
// any other part of the tag.
func metaAttrSpan(tag, want string) (int, int, bool) {
	i := len("<meta")
	n := len(tag)
	for i < n {
		for i < n && isHTMLSpace(tag[i]) {
			i++
		}
		if i >= n || tag[i] == '>' || tag[i] == '/' {
			break
		}
		start := i
		for i < n && !isHTMLSpace(tag[i]) && tag[i] != '=' && tag[i] != '>' {
			i++
		}
		name := strings.ToLower(tag[start:i])
		for i < n && isHTMLSpace(tag[i]) {
			i++
		}
		if i >= n || tag[i] != '=' {
			if name == want {
				return 0, 0, false
			}
			continue
		}
		i++
		for i < n && isHTMLSpace(tag[i]) {
			i++
		}
		if i < n && (tag[i] == '"' || tag[i] == '\'') {
			q := tag[i]
			i++
			vs := i
			for i < n && tag[i] != q {
				i++
			}
			if name == want {
				return vs, i, true
			}
			if i < n {
				i++
			}
		} else {
			vs := i
			for i < n && !isHTMLSpace(tag[i]) && tag[i] != '>' {
				i++
			}
			if name == want {
				return vs, i, true
			}
		}
	}
	return 0, 0, false
}

// metaCharsetFromHTML returns the first real charset declaration in the
// document. Only a charset attribute or an http-equiv=Content-Type meta is
// considered; text inside a description/content attribute is ignored.
func metaCharsetFromHTML(html string) string {
	for _, tag := range metaTagRe.FindAllString(html, -1) {
		if v, ok := metaAttrValue(tag, "charset"); ok && v != "" {
			return v
		}
		if v, ok := metaAttrValue(tag, "http-equiv"); ok && strings.EqualFold(v, "Content-Type") {
			if c, ok := metaAttrValue(tag, "content"); ok {
				if cs := charsetFromContentType(c); cs != "" {
					return cs
				}
			}
		}
	}
	return ""
}

// rewriteCharsetMeta updates legacy charset declarations after transcoding so
// the browser cannot override the UTF-8 response with the original GBK/etc.
// This mirrors the upstream `_0x504211` helper. Only real charset declarations
// are touched: a description/OG tag containing the literal text "charset=..."
// must not have its visible content rewritten.
func rewriteCharsetMeta(html string) string {
	return metaTagRe.ReplaceAllStringFunc(html, func(tag string) string {
		if v, ok := metaAttrValue(tag, "http-equiv"); ok && strings.EqualFold(v, "Content-Type") {
			return charsetMetaSnippet
		}
		if _, ok := metaAttrValue(tag, "charset"); !ok {
			return tag
		}
		start, end, ok := metaAttrSpan(tag, "charset")
		if !ok {
			return tag
		}
		return tag[:start] + "UTF-8" + tag[end:]
	})
}

// decodeHTMLBody transcodes a raw HTML body to a UTF-8 string.
// The declared charset is resolved from (in order): the Content-Type header,
// a BOM, the <meta charset> / <meta http-equiv=Content-Type> tag.
// The second return value is the charset actually used ("" for UTF-8).
func decodeHTMLBody(raw []byte, contentTypeHeader string) (string, string) {
	// BOM sniffing first: UTF-16 BOMs cannot be reliably scanned as ASCII.
	switch {
	case bytes.HasPrefix(raw, bomUTF16LE):
		return decodeUTF16(raw, "utf-16le")
	case bytes.HasPrefix(raw, bomUTF16BE):
		return decodeUTF16(raw, "utf-16be")
	case bytes.HasPrefix(raw, bomUTF8):
		return string(bytes.TrimPrefix(raw, bomUTF8)), ""
	}
	// The upstream node pipeline gives an in-document meta charset priority
	// over the HTTP header (browsers do the same for legacy documents).
	window := raw
	if len(window) > sniffWindowBytes {
		window = window[:sniffWindowBytes]
	}
	if cs := metaCharsetFromHTML(string(window)); cs != "" {
		if !isUTF8(cs) {
			return decodeBytes(raw, cs)
		}
		return string(raw), ""
	}
	if m := xmlEncodingRe.FindSubmatch(window); m != nil {
		cs := string(m[1])
		if !isUTF8(cs) {
			return decodeBytes(raw, cs)
		}
	}
	if cs := charsetFromContentType(contentTypeHeader); cs != "" && !isUTF8(cs) {
		return decodeBytes(raw, cs)
	}
	return string(raw), ""
}

// htmlBodyCharset reports the charset a browser will effectively use for raw
// (untranscoded) HTML bytes. It follows the same priority as decodeHTMLBody:
// BOM, in-document meta / xml declaration, then the Content-Type header. The
// boolean reports a UTF-16 body, into which ASCII bytes cannot be injected.
func htmlBodyCharset(raw []byte, contentTypeHeader string) (string, bool) {
	switch {
	case bytes.HasPrefix(raw, bomUTF16LE):
		return "utf-16le", true
	case bytes.HasPrefix(raw, bomUTF16BE):
		return "utf-16be", true
	case bytes.HasPrefix(raw, bomUTF8):
		return "utf-8", false
	}
	window := raw
	if len(window) > sniffWindowBytes {
		window = window[:sniffWindowBytes]
	}
	if cs := metaCharsetFromHTML(string(window)); cs != "" {
		return cs, utf16CharsetNames.MatchString(cs)
	}
	if m := xmlEncodingRe.FindSubmatch(window); m != nil {
		cs := string(m[1])
		return cs, utf16CharsetNames.MatchString(cs)
	}
	if cs := charsetFromContentType(contentTypeHeader); cs != "" {
		return cs, utf16CharsetNames.MatchString(cs)
	}
	return "", false
}

// decodeTextBody transcodes a plain-text (CSS/JS) body to UTF-8. The declared
// HTTP charset wins; when it is absent, a CSS @charset declaration and byte
// order marks are honoured (UTF-16 files otherwise cannot be prepended to with
// the ASCII bootstrap line).
func decodeTextBody(raw []byte, contentTypeHeader string) ([]byte, string) {
	switch {
	case bytes.HasPrefix(raw, bomUTF16LE):
		out, used := decodeUTF16(raw, "utf-16le")
		return []byte(out), used
	case bytes.HasPrefix(raw, bomUTF16BE):
		out, used := decodeUTF16(raw, "utf-16be")
		return []byte(out), used
	case bytes.HasPrefix(raw, bomUTF8):
		raw = bytes.TrimPrefix(raw, bomUTF8)
	}
	cs := charsetFromContentType(contentTypeHeader)
	if cs == "" {
		if m := cssCharsetRe.FindSubmatch(raw); m != nil {
			cs = string(m[1])
		}
	}
	if cs == "" || isUTF8(cs) {
		return raw, ""
	}
	out, used := decodeBytes(raw, cs)
	return []byte(out), used
}

// rewriteCSSCharset replaces a leading @charset declaration with UTF-8 after
// the body was transcoded, so CSS clients do not re-decode the UTF-8 bytes
// with the old legacy encoding.
func rewriteCSSCharset(css []byte) []byte {
	s := string(css)
	if loc := cssCharsetRe.FindStringIndex(s); loc != nil {
		s = `@charset "UTF-8";` + s[loc[1]:]
	}
	return []byte(s)
}

// charsetFromContentType extracts charset=xxx from a Content-Type header,
// accepting optional whitespace around '=' (e.g. "charset = gbk").
func charsetFromContentType(ct string) string {
	for _, part := range strings.Split(ct, ";") {
		part = strings.TrimSpace(part)
		if m := charsetParamRe.FindStringSubmatch(part); m != nil {
			return strings.Trim(m[1], `"' `)
		}
	}
	return ""
}

func isUTF8(cs string) bool {
	cs = strings.ToLower(strings.TrimSpace(cs))
	return cs == "" || cs == "utf8" || cs == "utf-8" || cs == "us-ascii" || cs == "ascii"
}

// decodeBytes converts raw bytes in charset cs to a UTF-8 string. When the
// charset is unknown or decoding fails it returns the original bytes and an
// empty used value, so callers keep the original encoding instead of
// advertising UTF-8 for replacement-character data.
func decodeBytes(raw []byte, cs string) (string, string) {
	cs = normalizeCharset(cs)

	if utf16CharsetNames.MatchString(cs) {
		return decodeUTF16(raw, cs)
	}

	enc, err := htmlindex.Get(cs)
	if err != nil {
		// Unknown charset: keep the original bytes. Returning a
		// replacement-character UTF-8 string here would make the caller
		// advertise UTF-8 and destroy data we never decoded.
		return string(raw), ""
	}
	out, err := enc.NewDecoder().Bytes(raw)
	if err != nil {
		return string(raw), ""
	}
	return string(out), cs
}

func decodeUTF16(raw []byte, cs string) (string, string) {
	orig := raw
	var name string
	switch {
	case bytes.HasPrefix(raw, bomUTF16LE):
		name = "UTF-16LE"
		raw = raw[2:]
	case bytes.HasPrefix(raw, bomUTF16BE):
		name = "UTF-16BE"
		raw = raw[2:]
	case strings.HasSuffix(cs, "le"):
		name = "UTF-16LE"
	case strings.HasSuffix(cs, "be"):
		name = "UTF-16BE"
	default:
		name = "UTF-16BE" // RFC 2781 default without BOM
	}
	enc, _ := htmlindex.Get(name)
	if enc == nil {
		enc = unicode.UTF16(unicode.BigEndian, unicode.UseBOM)
	}
	out, err := enc.NewDecoder().Bytes(raw)
	if err != nil {
		return string(orig), ""
	}
	return string(out), name
}

// normalizeCharset maps common aliases to what x/text understands.
func normalizeCharset(cs string) string {
	cs = strings.ToLower(strings.TrimSpace(cs))
	cs = strings.ReplaceAll(cs, "_", "-")
	switch cs {
	case "gb2312", "gbk", "cp936", "gb18030", "zh-cn", "gb231280":
		return "gb18030" // superset, safe round-trip
	case "shift-jis", "sjis", "shiftjs", "ms932", "cp932":
		return "shift-jis"
	case "eucjp", "euc-jp":
		return "euc-jp"
	case "euckr", "euc-kr", "cp949":
		return "euc-kr"
	case "big5", "big5-hkscs", "cp950":
		return "big5"
	case "latin1", "l1", "cp1252", "windows-1252":
		return "windows-1252"
	case "cp1251", "windows-1251":
		return "windows-1251"
	case "koi8r", "koi8-r":
		return "koi8-r"
	case "koi8u", "koi8-u":
		return "koi8-u"
	}
	return cs
}

// decodeHTMLBodyWithSniff is kept for future use: it falls back to a UTF-8
// validity check before declaring ISO-8859-1.
var _ = supportedCharset
