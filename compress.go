package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"io"

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

func normalizeEncoding(e string) string {
	// Content-Encoding may list multiple codings ("gzip, br") though rare.
	if i := bytes.IndexByte([]byte(e), ','); i >= 0 {
		e = e[:i]
	}
	switch e {
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
