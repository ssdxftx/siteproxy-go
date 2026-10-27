package main

import "strings"

// rewriteJSIdentifiers rewrites the identifiers the original siteproxy
// rewrites in node mode, while leaving string literals, comments and regex
// literals untouched:
//
//	location                          -> ___location
//	navigator.serviceWorker           -> navigator.___serviceWorker
//	document.requestStorageAccessFor  -> document.___requestStorageAccessFor
//	window.location.href              -> window.___location.href
//
// The scanner is a port of upstream `_0x51f5a2`. It exists because the Go
// server previously only rewrote `window.location` assignments, so bare
// `location.href = ...` navigations escaped the proxy and
// `navigator.serviceWorker.register(...)` threw a TypeError.
func rewriteJSIdentifiers(src string) string {
	return rewriteOutsideJSLiterals(src, rewriteIdentifierSegment)
}

func rewriteIdentifierSegment(code string) string {
	code = replaceWordPhrase(code, "navigator.serviceWorker", "navigator.___serviceWorker")
	code = replaceWordPhrase(code, "document.requestStorageAccessFor", "document.___requestStorageAccessFor")
	code = replaceBareLocation(code)
	return strings.ReplaceAll(code, "window.location.href", "window.___location.href")
}

func isJSIdentChar(c byte) bool {
	return c == '_' || c == '$' ||
		(c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z')
}

func replaceWordPhrase(s, phrase, repl string) string {
	if !strings.Contains(s, phrase) {
		return s
	}
	var b strings.Builder
	last := 0
	for {
		rel := strings.Index(s[last:], phrase)
		if rel < 0 {
			b.WriteString(s[last:])
			break
		}
		i := last + rel
		end := i + len(phrase)
		prevOK := i == 0 || !isJSIdentChar(s[i-1])
		nextOK := end == len(s) || !isJSIdentChar(s[end])
		if prevOK && nextOK {
			b.WriteString(s[last:i])
			b.WriteString(repl)
			last = end
		} else {
			b.WriteString(s[last:end])
			last = end
		}
	}
	return b.String()
}

// replaceBareLocation mirrors upstream regex (?<![-\w])location(?![-\w]).
func replaceBareLocation(s string) string {
	if !strings.Contains(s, "location") {
		return s
	}
	var b strings.Builder
	last := 0
	for {
		rel := strings.Index(s[last:], "location")
		if rel < 0 {
			b.WriteString(s[last:])
			break
		}
		i := last + rel
		end := i + len("location")
		prevOK := i == 0 || (!isJSIdentChar(s[i-1]) && s[i-1] != '-')
		nextOK := end == len(s) || (!isJSIdentChar(s[end]) && s[end] != '-')
		if prevOK && nextOK {
			b.WriteString(s[last:i])
			b.WriteString("___location")
			last = end
		} else {
			b.WriteString(s[last:end])
			last = end
		}
	}
	return b.String()
}

// rewriteOutsideJSLiterals applies fn to code segments and copies strings,
// comments and regex literals verbatim.
func rewriteOutsideJSLiterals(src string, fn func(string) string) string {
	n := len(src)
	var out strings.Builder
	last := 0
	i := 0
	lastSig := byte(';')

	flush := func(end int) {
		if last < end {
			out.WriteString(fn(src[last:end]))
		}
		last = end
	}
	copyRaw := func(end int) {
		if last < end {
			out.WriteString(src[last:end])
		}
		last = end
	}

	for i < n {
		c := src[i]

		if c == '/' && i+1 < n && src[i+1] == '*' {
			flush(i)
			j := i + 2
			for j+1 < n {
				if src[j] == '*' && src[j+1] == '/' {
					j += 2
					break
				}
				j++
			}
			if j > n {
				j = n
			}
			copyRaw(j)
			i = j
			lastSig = '/'
			continue
		}

		if c == '/' && i+1 < n && src[i+1] == '/' {
			flush(i)
			j := i + 2
			for j < n && src[j] != '\n' {
				j++
			}
			copyRaw(j)
			i = j
			lastSig = '/'
			continue
		}

		if c == '/' && (strings.IndexByte("(,;:!&|?=+-*~<>{}[]^%", lastSig) >= 0 || isRegexAllowed(src, i)) {
			if end, ok := scanRegexLiteral(src, i); ok {
				flush(i)
				copyRaw(end)
				i = end
				lastSig = '/'
				continue
			}
		}

		if c == '"' || c == '\'' {
			flush(i)
			j := skipQuoted(src, i)
			copyRaw(j)
			i = j
			lastSig = ')'
			continue
		}

		if c == '`' {
			flush(i)
			j := i + 1
			var tb strings.Builder
			tb.WriteByte('`')
			segStart := j
			for j < n {
				switch {
				case src[j] == '\\' && j+1 < n:
					j += 2
				case src[j] == '`':
					tb.WriteString(src[segStart:j])
					tb.WriteByte('`')
					j++
					goto templateDone
				case src[j] == '$' && j+1 < n && src[j+1] == '{':
					tb.WriteString(src[segStart : j+2])
					close := findMatchingBrace(src, j+2)
					if close < 0 {
						tb.WriteString(src[j+2:])
						j = n
						goto templateDone
					}
					tb.WriteString(rewriteOutsideJSLiterals(src[j+2:close], fn))
					tb.WriteByte('}')
					j = close + 1
					segStart = j
				default:
					j++
				}
			}
			tb.WriteString(src[segStart:])
		templateDone:
			out.WriteString(tb.String())
			last = j
			i = j
			lastSig = ')'
			continue
		}

		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			lastSig = c
		}
		i++
	}

	flush(n)
	return out.String()
}

// isRegexAllowed mirrors upstream `_0x9113c4`: a `/` starts a regex literal
// when it follows an operator/keyword rather than a value.
func isRegexAllowed(src string, slash int) bool {
	j := slash - 1
	for j >= 0 && isJSSpace(src[j]) {
		j--
	}
	if j < 0 || !(src[j] == '_' || src[j] == '$' ||
		(src[j] >= 'a' && src[j] <= 'z') || (src[j] >= 'A' && src[j] <= 'Z')) {
		return false
	}
	end := j + 1
	for j >= 0 && isJSIdentChar(src[j]) {
		j--
	}
	word := src[j+1 : end]
	switch word {
	case "return", "throw", "typeof", "delete", "instanceof", "in", "of",
		"new", "void", "yield", "await", "case", "do", "else":
		return j < 0 || src[j] != '.'
	}
	return false
}

func isJSSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// scanRegexLiteral returns the end index of a regex literal starting at start,
// or ok=false when the slash is actually division.
func scanRegexLiteral(src string, start int) (int, bool) {
	n := len(src)
	j := start + 1
	inClass := false
	for j < n {
		c := src[j]
		if c == '\\' && j+1 < n {
			j += 2
			continue
		}
		if c == '\n' {
			return 0, false
		}
		if c == '[' {
			inClass = true
		} else if c == ']' {
			inClass = false
		} else if c == '/' && !inClass {
			end := j + 1
			for end < n && strings.IndexByte("dgimsuvy", src[end]) >= 0 {
				end++
			}
			if end == n {
				return end, true
			}
			if strings.IndexByte(" \t\r\n\f\v)]},;:!&|?+-*=<>}.", src[end]) >= 0 {
				return end, true
			}
			return 0, false
		}
		j++
	}
	return 0, false
}

func skipQuoted(src string, start int) int {
	n := len(src)
	q := src[start]
	j := start + 1
	for j < n {
		if src[j] == '\\' && j+1 < n {
			j += 2
			continue
		}
		if src[j] == q {
			return j + 1
		}
		j++
	}
	return n
}

func skipTemplate(src string, start int) int {
	n := len(src)
	j := start + 1
	for j < n {
		if src[j] == '\\' && j+1 < n {
			j += 2
			continue
		}
		if src[j] == '`' {
			return j + 1
		}
		if src[j] == '$' && j+1 < n && src[j+1] == '{' {
			close := findMatchingBrace(src, j+2)
			if close < 0 {
				return n
			}
			j = close + 1
			continue
		}
		j++
	}
	return n
}

// findMatchingBrace returns the index of the `}` matching the `{` before
// start, skipping nested strings/templates/comments/regex literals.
func findMatchingBrace(src string, start int) int {
	n := len(src)
	depth := 1
	i := start
	lastSig := byte(';')
	for i < n {
		c := src[i]
		switch {
		case c == '"' || c == '\'':
			i = skipQuoted(src, i)
			lastSig = ')'
		case c == '`':
			i = skipTemplate(src, i)
			lastSig = ')'
		case c == '/' && i+1 < n && src[i+1] == '*':
			i += 2
			for i+1 < n && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i += 2
			lastSig = '/'
		case c == '/' && i+1 < n && src[i+1] == '/':
			i += 2
			for i < n && src[i] != '\n' {
				i++
			}
			lastSig = '/'
		case c == '/':
			// Only treat `/` as a regex literal when the preceding token
			// allows one; otherwise it is division and must not swallow a
			// following `}` (which would desynchronise template parsing).
			if strings.IndexByte("(,;:!&|?=+-*~<>{}[]^%", lastSig) >= 0 || isRegexAllowed(src, i) {
				if end, ok := scanRegexLiteral(src, i); ok {
					i = end
					lastSig = '/'
					continue
				}
			}
			i++
			lastSig = '/'
		case c == '{':
			depth++
			i++
			lastSig = '{'
		case c == '}':
			depth--
			if depth == 0 {
				return i
			}
			i++
			lastSig = '}'
		default:
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' && c != '\f' && c != '\v' {
				lastSig = c
			}
			i++
		}
	}
	return -1
}
