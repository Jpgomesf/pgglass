package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	authEnv           = "PGGLASS_HTTP_TOKEN"
	defaultHTTPHost   = "127.0.0.1"
	defaultHTTPPort   = "8411"
	mcpPath           = "/mcp"
	listenMethod      = "subscriptions/listen"
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 10 * time.Second
)

func httpAddress(value, token string) (string, error) {
	if value == "" {
		return "", nil
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", fmt.Errorf("--http %q: want [host]:port, such as :8411 or 127.0.0.1:8411", value)
	}
	if host == "" {
		host = defaultHTTPHost
	}
	if port == "" {
		port = defaultHTTPPort
	}
	if token == "" && !isLoopback(host) {
		return "", fmt.Errorf("--http %q listens beyond this machine: set %s to a secret token that every request must then carry", value, authEnv)
	}
	return net.JoinHostPort(host, port), nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

func listenNetwork(addr string) string {
	host, _, _ := net.SplitHostPort(addr)
	if ip, err := netip.ParseAddr(host); err == nil && ip.Unmap().Is4() {
		return "tcp4"
	}
	return "tcp"
}

func serveHTTP(ctx context.Context, server *mcp.Server, addr, token string, logger *slog.Logger) error {
	ln, err := net.Listen(listenNetwork(addr), addr)
	if err != nil {
		return err
	}
	stopped, stop := context.WithCancel(context.Background())
	defer stop()
	server.AddReceivingMiddleware(endCalls(ctx, stopped))
	srv := &http.Server{
		Handler:                      httpHandler(server, token),
		ReadHeaderTimeout:            readHeaderTimeout,
		DisableGeneralOptionsHandler: true,
	}
	logger.Info("listening", slog.String("url", "http://"+ln.Addr().String()+mcpPath), slog.Bool("token_required", token != ""))
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("shutdown: cancelled the calls still running after %s", shutdownTimeout)
	}
	return err
}

func endCalls(shutdown, stopped context.Context) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			end := stopped
			if method == listenMethod {
				end = shutdown
			}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			defer context.AfterFunc(end, cancel)()
			return next(ctx, method, req)
		}
	}
}

func httpHandler(server *mcp.Server, token string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(mcpPath, mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, PropagateRequestCancellation: true}))
	var handler http.Handler = mux
	if token != "" {
		handler = requireToken(token, handler)
	}
	return loopbackOrigins(handler)
}

func requireToken(token string, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fields := strings.Fields(r.Header.Get("Authorization"))
		if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
			unauthorized(w, `Bearer realm="pgglass"`, "no bearer token")
			return
		}
		got := sha256.Sum256([]byte(fields[1]))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			unauthorized(w, `Bearer realm="pgglass", error="invalid_token"`, "invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func unauthorized(w http.ResponseWriter, challenge, reason string) {
	w.Header().Set("WWW-Authenticate", challenge)
	http.Error(w, reason, http.StatusUnauthorized)
}

func loopbackOrigins(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && !loopbackOrigin(origin) {
			http.Error(w, "Forbidden: only loopback origins may call this server", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && isLoopback(u.Hostname())
}
