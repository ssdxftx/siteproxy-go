package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNonRedirect3xxKeepsBodyAndLocation covers 300 Multiple Choices: the
// upstream only treats 301/302/303/307/308 as redirects. Other 3xx responses
// keep their body (rewritten like a normal document) and their Location
// header verbatim.
func TestNonRedirect3xxKeepsBodyAndLocation(t *testing.T) {
	const page = `<html><body><a href="/x">x</a></body></html>`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Location", "/loc")
		w.WriteHeader(http.StatusMultipleChoices)
		fmt.Fprint(w, page)
	}))
	defer up.Close()

	ts, base := startProxy(t, up.URL)
	resp, body := proxyGetNoRedirect(t, ts, base+"/choice")
	if resp == nil {
		t.Fatal("nil response")
	}
	if resp.StatusCode != http.StatusMultipleChoices {
		t.Fatalf("status = %d, want 300", resp.StatusCode)
	}
	if len(body) == 0 {
		t.Fatal("300 body was dropped")
	}
	if !strings.Contains(string(body), base+"/x") {
		t.Errorf("300 body was not rewritten: %s", body)
	}
	if !strings.Contains(string(body), "siteproxy-response-injected.js") {
		t.Errorf("300 body was not injected: %s", body)
	}
	if loc := resp.Header.Get("Location"); loc != "/loc" {
		t.Errorf("Location = %q, want the origin value /loc", loc)
	}
}

// TestPartialContentStreamedVerbatim: a 206 body is a byte range. Rewriting
// it would corrupt the payload and break Content-Range/Content-Length.
func TestPartialContentStreamedVerbatim(t *testing.T) {
	const page = `<html><body><a href="/x">partial</a></body></html>`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Range", "bytes 0-9/100")
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, page)
	}))
	defer up.Close()

	ts, base := startProxy(t, up.URL)
	resp, body := proxyGetNoRedirect(t, ts, base+"/range")
	if resp == nil {
		t.Fatal("nil response")
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if string(body) != page {
		t.Errorf("206 body was modified:\n got %q\nwant %q", body, page)
	}
	if strings.Contains(string(body), "siteproxy-response-injected.js") {
		t.Error("206 body was injected")
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 0-9/100" {
		t.Errorf("Content-Range = %q", cr)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

// TestUntypedHTMLIsSniffedNotForcedToPlainText: the proxy must not invent a
// text/plain Content-Type for an untyped body. Browsers (and net/http) sniff
// HTML, so forcing text/plain made untyped HTML render as source text.
func TestUntypedHTMLIsSniffedNotForcedToPlainText(t *testing.T) {
	const page = `<html><body><a href="/x">x</a></body></html>`
	// Raw-socket upstream: Go's httptest handler sniffs and adds a
	// Content-Type itself, which would hide the missing-header path.
	upList, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upList.Close()
	upURL := "http://" + upList.Addr().String()
	go func() {
		for {
			conn, err := upList.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(page), page)
					if req.Close {
						return
					}
				}
			}(conn)
		}
	}()

	ts, base := startProxy(t, upURL)
	resp, body := proxyGet(ts, base+"/untyped", nil)
	if resp == nil {
		t.Fatal("nil response")
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "text/html") {
		t.Errorf("untyped HTML Content-Type = %q, want empty or sniffed text/html", ct)
	}
	if string(body) != page {
		t.Errorf("untyped body was rewritten/injected:\n got %q\nwant %q", body, page)
	}
}

// TestCorruptGzipPassesThroughWithoutHang: gzip.NewReader consumes the
// stream header before reporting corruption. The verbatim fallback must
// replay those bytes, otherwise Content-Length promises more than is sent
// and the client hangs.
func TestCorruptGzipPassesThroughWithoutHang(t *testing.T) {
	const raw = "notgzip"
	upList, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upList.Close()
	upURL := "http://" + upList.Addr().String()
	go func() {
		for {
			conn, err := upList.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: text/html\r\n"+
						"Content-Encoding: gzip\r\nContent-Length: %d\r\n\r\n%s", len(raw), raw)
					if req.Close {
						return
					}
				}
			}(conn)
		}
	}()

	ts, base := startProxy(t, upURL)
	client := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{DisableCompression: true},
	}
	resp, err := client.Get(ts.URL + base + "/corrupt")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != raw {
		t.Errorf("body = %q, want %q", body, raw)
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", ce)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "7" {
		t.Errorf("Content-Length = %q, want 7", cl)
	}
}

// TestWebSocketOriginUsesPageOrigin: a browser sends the proxy origin on a
// WebSocket handshake. The upstream must see the page's real origin (from the
// proxied Referer), not the target host's origin, so cross-origin sockets
// pass Origin checks.
func TestWebSocketOriginUsesPageOrigin(t *testing.T) {
	got := make(chan string, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWebSocketUpgrade(r) {
			http.Error(w, "not ws", 400)
			return
		}
		got <- r.Header.Get("Origin")
		hj := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		defer conn.Close()
		fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"+
			"Connection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
			wsAccept(r.Header.Get("Sec-WebSocket-Key")))
	}))
	defer up.Close()

	ts, base := startProxy(t, up.URL)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + base + "/echo"
	hdr := http.Header{}
	hdr.Set("Upgrade", "websocket")
	hdr.Set("Connection", "Upgrade")
	hdr.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	hdr.Set("Sec-WebSocket-Version", "13")
	hdr.Set("Origin", ts.URL)
	hdr.Set("Referer", ts.URL+token+"http/other.example/page")
	req := &http.Request{Method: "GET", URL: mustParse(wsURL), Header: hdr}

	conn, err := netDialTCP(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	select {
	case origin := <-got:
		if origin != "http://other.example" {
			t.Errorf("upstream Origin = %q, want page origin http://other.example", origin)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not receive a handshake")
	}
}

// TestCSSCharsetHeaderNormalizesAtCharset: a UTF-8 HTTP header wins over a
// stale @charset, but the declaration must be rewritten too or the browser
// re-decodes the UTF-8 bytes as GBK.
func TestCSSCharsetHeaderNormalizesAtCharset(t *testing.T) {
	const css = `@charset "gbk"; .a{color:red}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		fmt.Fprint(w, css)
	}))
	defer up.Close()

	ts, base := startProxy(t, up.URL)
	resp, body := proxyGet(ts, base+"/style.css", nil)
	if resp == nil {
		t.Fatal("nil response")
	}
	if !strings.HasPrefix(string(body), `@charset "UTF-8";`) {
		t.Errorf("@charset not normalised: %q", body)
	}
}

// TestAttachmentIsNotRewritten: Content-Disposition: attachment is a file
// download; injecting the runtime or rewriting URLs changes the saved file.
func TestAttachmentIsNotRewritten(t *testing.T) {
	const page = `<html><body><a href="/x">x</a></body></html>`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Disposition", `attachment; filename="a.html"`)
		fmt.Fprint(w, page)
	}))
	defer up.Close()

	ts, base := startProxy(t, up.URL)
	resp, body := proxyGet(ts, base+"/download", nil)
	if resp == nil {
		t.Fatal("nil response")
	}
	if string(body) != page {
		t.Errorf("attachment was modified:\n got %q\nwant %q", body, page)
	}
	if strings.Contains(string(body), "siteproxy-response-injected.js") {
		t.Error("attachment was injected")
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q", cd)
	}
}

// TestNotModifiedLocationUntouched: 304 is not a redirect for header-rewrite
// purposes; Location must pass through unchanged.
func TestNotModifiedLocationUntouched(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://ex.example/loc")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer up.Close()

	ts, base := startProxy(t, up.URL)
	resp, _ := proxyGetNoRedirect(t, ts, base+"/cached")
	if resp == nil {
		t.Fatal("nil response")
	}
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "https://ex.example/loc" {
		t.Errorf("304 Location = %q, want the origin value", loc)
	}
}
