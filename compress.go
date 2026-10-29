package main

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"io"
	"strings"

	"github.com/andybalholm/brotli"
)

// newDecoder returns a decompressing reader for the given Content-Encoding
// value ("gzip", "deflate", "br").
func newDecoder(encoding string, r io.Reader) (io.Reader, bool) {
	switch normalizeEncoding(encoding) {
	case "gzip":
		if gr, err := gzip.NewReader(bufio.NewReaderSize(r, 64<<10)); err == nil {
			return gr, true
		}
	case "deflate":
		// Servers may send zlib-wrapped or raw deflate; peek to pick.
		br := bufio.NewReaderSize(r, 64<<10)
		head, _ := br.Peek(2)
		if len(head) == 2 && head[0] == 0x78 {
			if zr, err := zlibReader(br); err == nil {
				return zr, true
			}
		}
		return flate.NewReader(br), true
	case "br":
		return brotli.NewReader(bufio.NewReaderSize(r, 64<<10)), true
	}
	return nil, false
}

// newDecoderChain applies every coding in a Content-Encoding list in reverse
// order. HTTP codings are listed in the order they were applied, so decoding
// must start with the last one ("br, gzip" -> ungzip, then unbrotli).
func newDecoderChain(encoding string, r io.Reader) (io.Reader, bool) {
	parts := strings.Split(encoding, ",")
	encs := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || strings.EqualFold(p, "identity") {
			continue
		}
		encs = append(encs, p)
	}
	if len(encs) == 0 {
		return r, true
	}
	body := r
	for i := len(encs) - 1; i >= 0; i-- {
		dec, ok := newDecoder(encs[i], body)
		if !ok {
			return nil, false
		}
		body = dec
	}
	return body, true
}

func normalizeEncoding(e string) string {
	switch strings.ToLower(strings.TrimSpace(e)) {
	case "gzip", "x-gzip":
		return "gzip"
	case "deflate":
		return "deflate"
	case "br":
		return "br"
	default:
		return e
	}
}
