package main

// ----------------------------------------------------------------------------
// Round-3 release-blocking audit tests:
//   - srcset parsing parity with the original v2.7.10 algorithm
//   - WebSocket control-cookie filtering + hijacked buffered bytes
//   - out-of-scope repair host validation (cookie poisoning)
//   - unsupported Content-Encoding passthrough (no CE stripping)
//   - empty / missing Content-Type handling (never rewritten as JS)
//   - empty HTML body is not injected into
//   - Connection-named hop-by-hop headers are dropped by name
// ----------------------------------------------------------------------------

import (
        "bufio"
        "fmt"
        "io"
        "net"
        "net/http"
        "net/http/httptest"
        "strings"
        "testing"
)

// --- 1. srcset ---------------------------------------------------------------

func TestSrcsetMinifiedComma(t *testing.T) {
        publicPrefix := "http://p.test/tok/"
        base := "http://p.test/tok/https/x.com/dir/page"
        got := rewriteSrcset("/a.png 1x,/b.png 2x", publicPrefix, base, "p.test")
        want := publicPrefix + "https/x.com/a.png 1x," + publicPrefix + "https/x.com/b.png 2x"
        if got != want {
                t.Errorf("minified srcset:\n got %q\nwant %q", got, want)
        }
}

func TestSrcsetCommaInsideURLKept(t *testing.T) {
        publicPrefix := "http://p.test/tok/"
        base := "http://p.test/tok/https/x.com/dir/page"
        got := rewriteSrcset("img.php?w=300,h=200 2x", publicPrefix, base, "p.test")
        want := publicPrefix + "https/x.com/dir/img.php?w=300,h=200 2x"
        if got != want {
                t.Errorf("comma-in-URL must be kept as one candidate:\n got %q\nwant %q", got, want)
        }
}

func TestSrcsetNoSpaceSingleToken(t *testing.T) {
        // "a.png,b.png" without whitespace is ONE url token (original behaviour:
        // only trailing commas are separators).
        publicPrefix := "http://p.test/tok/"
        base := "http://p.test/tok/https/x.com/dir/page"
        got := rewriteSrcset("a.png,b.png 2x", publicPrefix, base, "p.test")
        want := publicPrefix + "https/x.com/dir/a.png,b.png 2x"
        if got != want {
                t.Errorf("no-space comma list:\n got %q\nwant %q", got, want)
        }
}

func TestSrcsetTrailingCommaDescriptor(t *testing.T) {
        publicPrefix := "http://p.test/tok/"
        base := "http://p.test/tok/https/x.com/"
        got := rewriteSrcset("/a.png, /b.png 2x", publicPrefix, base, "p.test")
        want := publicPrefix + "https/x.com/a.png, " + publicPrefix + "https/x.com/b.png 2x"
        if got != want {
                t.Errorf("trailing-comma candidates:\n got %q\nwant %q", got, want)
        }
}

// --- 2. WebSocket ------------------------------------------------------------

func TestWebSocketControlCookieFilteredAndBufferedBytes(t *testing.T) {
        // The upstream echoes its received Cookie header back as a text frame,
        // then echoes every subsequent frame (RFC6455 minimal).
        var gotCookie = make(chan string, 1)
        up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                if !isWebSocketUpgrade(r) {
                        http.Error(w, "not ws", 400)
                        return
                }
                hj := w.(http.Hijacker)
                conn, rw, _ := hj.Hijack()
                defer conn.Close()
                // Complete the handshake.
                fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", wsAccept(r.Header.Get("Sec-WebSocket-Key")))
                frame := func(payload []byte) {
                        out := append([]byte{0x81, byte(len(payload))}, payload...)
                        conn.Write(out)
                }
                frame([]byte("COOKIE=" + r.Header.Get("Cookie")))
                // Echo loop: reads from the hijacked bufio so pipelined early frames
                // (buffered before the handshake finished) are still delivered.
                hdr := make([]byte, 2)
                for {
                        if _, err := io.ReadFull(rw.Reader, hdr); err != nil {
                                return
                        }
                        length := int(hdr[1] & 0x7f)
                        mask := make([]byte, 4)
                        if _, err := io.ReadFull(rw.Reader, mask); err != nil {
                                return
                        }
                        payload := make([]byte, length)
                        if _, err := io.ReadFull(rw.Reader, payload); err != nil {
                                return
                        }
                        for i := range payload {
                                payload[i] ^= mask[i%4]
                        }
                        frame(payload)
                }
        }))
        defer up.Close()

        ts, base := startProxy(t, up.URL)

        wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + base + "/echo"
        hdr := http.Header{}
        hdr.Set("Upgrade", "websocket")
        hdr.Set("Connection", "Upgrade")
        hdr.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
        hdr.Set("Sec-WebSocket-Version", "13")
        // Poison the outbound header with the proxy control cookies: they are
        // scoped to Path=/ on the proxy origin and must never reach upstream.
        hdr.Set("Cookie", "proxy_real_host="+strings.TrimPrefix(up.URL, "http://")+"; proxy_real_protocol=http; sid=keepme")
        req := &http.Request{Method: "GET", URL: mustParse(wsURL), Header: hdr}

        conn, err := netDialTCP(ts.URL)
        if err != nil {
                t.Fatal(err)
        }
        defer conn.Close()

        // Write the upgrade request AND an early data frame back-to-back in one
        // go, so the early frame lands in the proxy's hijacked bufio buffer.
        if err := req.Write(conn); err != nil {
                t.Fatal(err)
        }
        payload := []byte("early")
        mask := []byte{1, 2, 3, 4}
        frame := []byte{0x81, 0x80 | byte(len(payload))}
        frame = append(frame, mask...)
        for i, b := range payload {
                frame = append(frame, b^mask[i%4])
        }
        if _, err := conn.Write(frame); err != nil {
                t.Fatal(err)
        }

        br := bufio.NewReader(conn)
        resp, err := http.ReadResponse(br, req)
        if err != nil {
                t.Fatal(err)
        }
        if resp.StatusCode != http.StatusSwitchingProtocols {
                t.Fatalf("ws handshake status = %d", resp.StatusCode)
        }

        readFrame := func() string {
                h := make([]byte, 2)
                if _, err := io.ReadFull(br, h); err != nil {
                        t.Fatalf("frame header: %v", err)
                }
                b := make([]byte, int(h[1]&0x7f))
                if _, err := io.ReadFull(br, b); err != nil {
                        t.Fatalf("frame body: %v", err)
                }
                return string(b)
        }

        cookieFrame := readFrame()
        select {
        case gotCookie <- cookieFrame:
        default:
        }
        if !strings.Contains(cookieFrame, "sid=keepme") {
                t.Errorf("site cookie lost: %q", cookieFrame)
        }
        if strings.Contains(cookieFrame, "proxy_real_host=") || strings.Contains(cookieFrame, "proxy_real_protocol=") {
                t.Errorf("control cookies leaked to websocket upstream: %q", cookieFrame)
        }
        // The early pipelined frame must not be swallowed by the proxy.
        if f := readFrame(); f != "early" {
                t.Errorf("early pipelined frame lost (hijack buffer): got %q", f)
        }
}

// --- 3. out-of-scope repair host validation -----------------------------------

func TestRepairOutOfScopeHostValidation(t *testing.T) {
        var mu = make(chan struct{})
        up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                if r.URL.Path == "/attacker" {
                        fmt.Fprint(w, "PWNED")
                        mu <- struct{}{}
                        return
                }
                fmt.Fprint(w, "ok:"+r.URL.Path)
        }))
        defer up.Close()

        cfg := newTestConfig(0)
        cfg.ProxyURL = up.URL
        srv, err := NewServer(cfg)
        if err != nil {
                t.Fatal(err)
        }
        ts := httptest.NewServer(srv)
        defer ts.Close()

        // Poisoned control cookie: userinfo smuggle against the repair path.
        req, _ := http.NewRequest("GET", ts.URL+"/attacker", nil)
        req.Header.Set("Cookie", "proxy_real_protocol=https; proxy_real_host=x@evil.com")
        resp, err := http.DefaultClient.Do(req)
        if err != nil {
                t.Fatal(err)
        }
        body, _ := io.ReadAll(resp.Body)
        resp.Body.Close()
        if strings.Contains(string(body), "PWNED") {
                t.Errorf("poisoned proxy_real_host was used as upstream host")
        }
}

// --- 4. unsupported Content-Encoding ------------------------------------------

func TestUnsupportedEncodingStreamedVerbatim(t *testing.T) {
        up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                w.Header().Set("Content-Type", "text/html")
                w.Header().Set("Content-Encoding", "zstd")
                fmt.Fprint(w, `<html><body><a href="https://z.example.com/x">raw</a></body></html>`)
        }))
        defer up.Close()

        ts, base := startProxy(t, up.URL)
        resp, body := proxyGet(ts, base+"/z", nil)
        if resp == nil {
                t.Fatal("no response")
        }
        if resp.Header.Get("Content-Encoding") != "zstd" {
                t.Errorf("Content-Encoding must be preserved for undecodable streams, got %q", resp.Header.Get("Content-Encoding"))
        }
        if !strings.Contains(string(body), `href="https://z.example.com/x"`) {
                t.Errorf("body must be forwarded verbatim (no rewrite, no corruption): %q", body)
        }
        if strings.Contains(string(body), publicTokenMarker()) {
                t.Errorf("body must not be rewritten: %q", body)
        }
}

// publicTokenMarker returns the token prefix used in tests.
func publicTokenMarker() string { return token + "https/" }

// --- 5. empty / missing Content-Type -------------------------------------------

func TestEmptyContentTypeNotProcessed(t *testing.T) {
        // Raw-socket upstream: writes a response with NO Content-Type header at
        // all (Go's httptest handler would sniff one, hiding the real path).
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
                                        body := "BINARY\x00\x01\x02DATA<script>window.location='/x'</script>"
                                        fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s",
                                                len(body), body)
                                        if req.Close {
                                                return
                                        }
                                }
                        }(conn)
                }
        }()

        ts, base := startProxy(t, upURL)
        resp, body := proxyGet(ts, base+"/bin", nil)
        if resp == nil {
                t.Fatal("no response")
        }
        if !strings.Contains(string(body), "BINARY\x00\x01\x02DATA") {
                t.Errorf("untyped body must be forwarded verbatim: %q", body)
        }
        if strings.Contains(string(body), "___location") {
                t.Errorf("untyped body must not be treated as JavaScript: %q", body)
        }
        if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "text/plain") {
                t.Errorf("untyped body must stay untyped (or default to text/plain), got %q", ct)
        }
}

func TestEmptyHTMLBodyNoInjection(t *testing.T) {
        up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                w.Header().Set("Content-Type", "text/html; charset=UTF-8")
                w.WriteHeader(200) // empty body
        }))
        defer up.Close()

        ts, base := startProxy(t, up.URL)
        resp, body := proxyGet(ts, base+"/empty", nil)
        if resp == nil {
                t.Fatal("no response")
        }
        if len(body) != 0 {
                t.Errorf("empty upstream body must stay empty, got %q", body)
        }
}

// --- 6. Connection-named headers (upstream parity: they ARE forwarded) ---------

func TestConnectionNamedHeaderForwarded(t *testing.T) {
        up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                fmt.Fprint(w, "secret="+r.Header.Get("X-Secret-Token"))
        }))
        defer up.Close()

        ts, base := startProxy(t, up.URL)
        resp, body := proxyGet(ts, base+"/echohdr", map[string]string{
                "Connection":     "keep-alive, X-Secret-Token",
                "X-Secret-Token": "forwardme",
        })
        if resp == nil {
                t.Fatal("no response")
        }
        if !strings.Contains(string(body), "forwardme") {
                t.Errorf("Connection-named header must be forwarded like the original node server: %s", body)
        }
}

// --- 7. panic recovery sanity ---------------------------------------------------

func TestPanicRecorderImplementsHijacker(t *testing.T) {
        var pr interface{} = &panicRecorder{}
        if _, ok := pr.(http.Hijacker); !ok {
                t.Fatal("panicRecorder must implement http.Hijacker (websocket path)")
        }
        if _, ok := pr.(http.Flusher); !ok {
                t.Fatal("panicRecorder must implement http.Flusher (SSE path)")
        }
}

// --- 8. regression guard: empty body skip does not affect normal pages ----------

func TestNormalPageStillRewritten(t *testing.T) {
        up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                w.Header().Set("Content-Type", "text/html")
                fmt.Fprint(w, `<html><head></head><body><a href="https://ok.example.com/a">x</a></body></html>`)
        }))
        defer up.Close()

        ts, base := startProxy(t, up.URL)
        resp, body := proxyGet(ts, base+"/ok", nil)
        if resp == nil {
                t.Fatal("no response")
        }
        if !strings.Contains(string(body), publicTokenMarker()+"ok.example.com/a") {
                t.Errorf("normal page rewriting broke: %s", body)
        }
        if !strings.Contains(string(body), "siteproxy_service_worker.js") &&
                !strings.Contains(string(body), "siteproxy-response-injected.js") {
                t.Errorf("injection missing on normal page: %s", body)
        }
}
