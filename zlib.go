package main

import (
	"compress/zlib"
	"io"
)

func zlibReader(r io.Reader) (io.ReadCloser, error) {
	return zlib.NewReader(r)
}
