package main

import (
	"crypto/tls"
	"net"
	"time"
)

// tlsDialWithHost dials addr but validates the certificate against hostname
// (used for WebSocket upstreams whose SNI/Host differ from the dial target).
func tlsDialWithHost(dialer *net.Dialer, network, addr, hostname string) (net.Conn, error) {
	raw, err := dialer.Dial(network, addr)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{ServerName: hostname, InsecureSkipVerify: insecureSkipVerify()}
	conn := tls.Client(raw, cfg)
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		_ = raw.Close()
		return nil, err
	}
	if err := conn.Handshake(); err != nil {
		_ = raw.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}
