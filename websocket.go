package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ----------------------------------------------------------------------------
// WebSocket reverse proxy.
//
// Native Go gives us a clean bidirectional TCP pipe: hijack the client
// connection, dial the upstream with the original upgrade headers, then copy
// bytes both ways until either side disconnects.
// ----------------------------------------------------------------------------

func (p *Proxy) proxyWebSocket(w http.ResponseWriter, r *http.Request, target *TargetInfo) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "siteproxy: websocket not supported", http.StatusInternalServerError)
		return
	}
	// The hijacked bufio may already hold pipelined bytes (early WS frames
	// sent in the same TCP segment as the upgrade request): always pump
	// FROM the buffered reader, never from the raw conn, or those bytes
	// would be lost.
	clientConn, clientRW, err := hj.Hijack()
	if err != nil {
		http.Error(w, "siteproxy: hijack failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// Build the upstream dial address.
	scheme := "ws"
	if target.Protocol == "https" {
		scheme = "wss"
	}
	wsURL := scheme + "://" + target.Host + target.Path
	if target.RawQuery != "" {
		wsURL += "?" + target.RawQuery
	}
	u, err := url.Parse(wsURL)
	if err != nil {
		writeRawResponse(clientConn, "400 Bad Request", "invalid websocket target")
		return
	}

	hostPort := target.Host
	needsPort := !strings.Contains(hostPort, ":") ||
		(strings.HasPrefix(hostPort, "[") && strings.HasSuffix(hostPort, "]"))
	if needsPort {
		if scheme == "wss" {
			hostPort += ":443"
		} else {
			hostPort += ":80"
		}
	}

	var upstream net.Conn
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	if scheme == "wss" {
		upstream, err = tlsDialWithHost(dialer, "tcp", hostPort, target.Host)
	} else {
		upstream, err = dialer.Dial("tcp", hostPort)
	}
	if err != nil {
		// Do not echo the dial error: it reveals the upstream host/port.
		log.Printf("[siteproxy] websocket dial failed for %s: %v", target.Host, err)
		writeRawResponse(clientConn, "502 Bad Gateway", "websocket dial failed")
		return
	}
	defer upstream.Close()

	// Serialise the upgrade request exactly as the browser sent it.
	realOrigin := r.Header.Get("Origin")
	if realOrigin != "" {
		if real := UnrewriteURLWithBase(realOrigin, p.cfg.TokenPrefix, p.cfg.BasePath()); real != realOrigin {
			realOrigin = originOf(real)
		} else if sameOrigin(realOrigin, p.cfg.ProxyURL) {
			// Browsers send the proxy origin for WebSocket handshakes.
			// Upstream sites commonly validate Origin, so present the
			// target origin instead of the proxy's.
			realOrigin = target.Protocol + "://" + target.Host
		}
	}

	// Control cookies (proxy_real_*) are scoped to Path=/ on the proxy
	// origin and must never reach the upstream site - same rule as the
	// plain-HTTP pipeline.
	cookieHeader := ""
	if ck := r.Header.Get("Cookie"); ck != "" {
		cookieHeader = filterControlCookies(ck)
	}

	var sb strings.Builder
	sb.WriteString("GET " + u.RequestURI() + " HTTP/1.1\r\n")
	sb.WriteString("Host: " + target.Host + "\r\n")
	if realOrigin != "" {
		sb.WriteString("Origin: " + realOrigin + "\r\n")
	}
	for _, h := range []string{"Upgrade", "Connection", "Sec-WebSocket-Key",
		"Sec-WebSocket-Version", "Sec-WebSocket-Extensions", "Sec-WebSocket-Protocol",
		"User-Agent", "Authorization"} {
		for _, v := range r.Header.Values(h) {
			sb.WriteString(h + ": " + v + "\r\n")
		}
	}
	if cookieHeader != "" {
		sb.WriteString("Cookie: " + cookieHeader + "\r\n")
	}
	sb.WriteString("\r\n")

	if _, err := upstream.Write([]byte(sb.String())); err != nil {
		writeRawResponse(clientConn, "502 Bad Gateway", "websocket handshake write failed")
		return
	}

	// Relay the upstream handshake response verbatim.
	upBuf := bufio.NewReaderSize(upstream, 32<<10)
	resp, err := http.ReadResponse(upBuf, nil)
	if err != nil {
		writeRawResponse(clientConn, "502 Bad Gateway", "websocket handshake read failed")
		return
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		status := fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		writeRawResponseBytes(clientConn, status, body)
		return
	}

	// Write 101 + upgrade headers to the client.
	var rb strings.Builder
	rb.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	for _, h := range []string{"Upgrade", "Connection", "Sec-WebSocket-Accept",
		"Sec-WebSocket-Protocol", "Sec-WebSocket-Extensions"} {
		for _, v := range resp.Header.Values(h) {
			rb.WriteString(h + ": " + v + "\r\n")
		}
	}
	for _, sc := range resp.Header.Values("Set-Cookie") {
		for _, one := range splitSetCookieHeader(sc) {
			if c := rewriteSetCookie(one, p.cfg.TokenPrefix, target.SitePrefix(), p.cfg); c != "" {
				rb.WriteString("Set-Cookie: " + c + "\r\n")
			}
		}
	}
	rb.WriteString("\r\n")
	if _, err := clientConn.Write([]byte(rb.String())); err != nil {
		return
	}

	// Bidirectional pump until either side disconnects.
	done := make(chan struct{}, 2)
	go pumpBytes(upstream, clientRW, done)
	go pumpBytes(clientConn, upBuf, done)
	<-done
}

func pumpBytes(dst io.WriteCloser, src io.Reader, done chan struct{}) {
	buf := make([]byte, 32*1024)
	_, _ = io.CopyBuffer(dst, src, buf)
	type deadlineSetter interface{ SetDeadline(time.Time) error }
	if ds, ok := dst.(deadlineSetter); ok {
		_ = ds.SetDeadline(time.Now())
	}
	done <- struct{}{}
}

func writeRawResponse(conn net.Conn, status, msg string) {
	writeRawResponseBytes(conn, status, []byte(msg))
}

func writeRawResponseBytes(conn net.Conn, status string, body []byte) {
	msg := "HTTP/1.1 " + status + "\r\nContent-Type: text/plain; charset=UTF-8\r\n" +
		"Content-Length: " + fmt.Sprint(len(body)) + "\r\nConnection: close\r\n\r\n" + string(body)
	_, _ = conn.Write([]byte(msg))
}
