package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
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

func serveHTTP(ctx context.Context, server *mcp.Server, addr, token string, logger *slog.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	server.AddReceivingMiddleware(endListensOn(ctx))
	srv := &http.Server{Handler: httpHandler(server, token), ReadHeaderTimeout: readHeaderTimeout}
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
	return srv.Shutdown(shutdownCtx)
}

func endListensOn(shutdown context.Context) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != listenMethod {
				return next(ctx, method, req)
			}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			defer context.AfterFunc(shutdown, cancel)()
			return next(ctx, method, req)
		}
	}
}

func httpHandler(server *mcp.Server, token string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(mcpPath, mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true}))
	var handler http.Handler = mux
	if token != "" {
		handler = auth.RequireBearerToken(tokenVerifier(token), &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(handler)
	}
	return loopbackOrigins(handler)
}

func tokenVerifier(token string) auth.TokenVerifier {
	want := sha256.Sum256([]byte(token))
	return func(_ context.Context, got string, _ *http.Request) (*auth.TokenInfo, error) {
		sum := sha256.Sum256([]byte(got))
		if subtle.ConstantTimeCompare(sum[:], want[:]) != 1 {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{}, nil
	}
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
