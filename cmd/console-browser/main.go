// console-browser is a private, ephemeral headed-browser broker. It must run
// behind the container network policy installed by deploy/console-browser.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/consolebrowser/hosted"
)

func readToken(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("broker token unavailable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 1025))
	if err != nil || len(raw) > 1024 {
		return "", errors.New("invalid broker token")
	}
	token := strings.TrimSpace(string(raw))
	if len(token) < 32 || len(token) > 256 || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("invalid broker token")
	}
	return token, nil
}
func run() error {
	listen := flag.String("listen", ":8320", "Private HTTP listen address")
	tokenPath := flag.String("token-file", "/run/secrets/console-browser-token", "Broker secret file")
	root := flag.String("profile-root", "/run/console-browser", "Temporary profile directory (mount tmpfs)")
	health := flag.Bool("healthcheck", false, "Check the local broker and exit")
	flag.Parse()
	token, err := readToken(*tokenPath)
	if err != nil {
		return err
	}
	if *health {
		_, port, err := net.SplitHostPort(*listen)
		if err != nil {
			return errors.New("invalid broker listen address")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:"+port+"/healthz", nil)
		if err != nil {
			return errors.New("invalid broker listen address")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		client := http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		res, err := client.Do(req)
		if err != nil {
			return errors.New("broker unavailable")
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return errors.New("broker unavailable")
		}
		return nil
	}
	broker, err := hosted.New(hosted.Config{Token: token, ProfileRoot: *root})
	if err != nil {
		return err
	}
	defer broker.Close()
	server := &http.Server{Addr: *listen, Handler: broker, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ended := make(chan error, 1)
	go func() { ended <- server.ListenAndServe() }()
	select {
	case err = <-ended:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("broker server unavailable")
		}
	case <-ctx.Done():
		broker.Close()
		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if server.Shutdown(shutdownCtx) != nil {
			_ = server.Close()
		}
	}
	return nil
}
func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}
