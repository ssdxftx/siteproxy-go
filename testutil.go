package main

import (
	"crypto/tls"
	"net"
	"net/url"
	"strings"
)

func mustParse(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

// netDialTCP parses the test server address and dials it (with TLS when the
// URL scheme is https).
func netDialTCP(serverURL string) (net.Conn, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	if u.Scheme == "https" {
		return tls.Dial("tcp", host, &tls.Config{InsecureSkipVerify: true})
	}
	return net.Dial("tcp", host)
}
