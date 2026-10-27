// Package main implements siteproxy-go, a native Go re-implementation of
// netptop/siteproxy (https://github.com/netptop/siteproxy).
//
// siteproxy-go is an online reverse proxy: users visit
//
//      https://your-proxy-domain.com/{token_prefix}https/{target-host}/{path}
//
// and the server fetches the target website, rewrites all URLs inside the
// HTML/CSS/JS payload so that every subsequent request stays inside the
// proxy, and injects client-side scripts that override browser location /
// fetch / XHR / WebSocket so JavaScript-driven navigation is proxied too.
package main

import (
        "flag"
        "fmt"
        "log"
        "net/http"
        "os"
        "os/signal"
        "path/filepath"
        "syscall"
        "time"
)

var (
        configFile = flag.String("config", "config.json", "path to config file")
        showVer    = flag.Bool("version", false, "print version and exit")
)

const version = "2.7.11-go.1.1.0"

func main() {
        flag.Parse()
        if *showVer {
                fmt.Println("siteproxy-go", version)
                return
        }

        cfg, err := LoadConfig(configPath(*configFile))
        if err != nil {
                log.Fatalf("[siteproxy] load config: %v", err)
        }

        srv, err := NewServer(cfg)
        if err != nil {
                log.Fatalf("[siteproxy] init server: %v", err)
        }

        httpSrv := &http.Server{
                Addr:              fmt.Sprintf(":%d", cfg.LocalListenPort),
                Handler:           srv,
                ReadHeaderTimeout: 30 * time.Second,
                IdleTimeout:       120 * time.Second,
        }

        log.Printf("[siteproxy] IS_NODE: false, version: %s", version)
        log.Printf("[siteproxy] Listening on http://localhost:%d", cfg.LocalListenPort)
        log.Printf("[siteproxy] proxy_url=%s token_prefix=%s", cfg.ProxyURL, cfg.TokenPrefix)

        // Graceful shutdown on SIGINT/SIGTERM.
        go waitSignal(httpSrv)

        if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
                log.Fatalf("[siteproxy] server error: %v", err)
        }
        log.Println("[siteproxy] server stopped")
}

// waitSignal blocks until SIGINT/SIGTERM then shuts the server down.
func waitSignal(srv *http.Server) {
        ch := make(chan os.Signal, 1)
        signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
        s := <-ch
        log.Printf("[siteproxy] received %v, shutting down...", s)
        _ = srv.Close()
}

// configPath returns the directory of the running executable, used to locate
// config.json when the binary is started from another working directory.
func configPath(name string) string {
        if filepath.IsAbs(name) {
                return name
        }
        exe, err := os.Executable()
        if err != nil {
                return name
        }
        p := filepath.Join(filepath.Dir(exe), name)
        if _, err := os.Stat(p); err == nil {
                return p
        }
        return name
}
