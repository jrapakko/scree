package mcpserver

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MinTokenLen is the shortest SCREE_HTTP_TOKEN accepted.
const MinTokenLen = 16

// CheckListenAddr refuses addresses that are not loopback unless
// allowRemote is set and a token of at least MinTokenLen is given. An empty
// host, 0.0.0.0 and :: count as remote. A token, when set, must be long
// enough on loopback too.
func CheckListenAddr(addr string, allowRemote bool, token string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen address %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	if !loopback && !allowRemote {
		return fmt.Errorf("refusing to listen on %s: not a loopback address; pass -allow-remote and set SCREE_HTTP_TOKEN", addr)
	}
	if !loopback && token == "" {
		return fmt.Errorf("refusing to listen on %s: -allow-remote needs SCREE_HTTP_TOKEN", addr)
	}
	if token != "" && len(token) < MinTokenLen {
		return fmt.Errorf("SCREE_HTTP_TOKEN is too short: use at least %d characters", MinTokenLen)
	}
	return nil
}

// HTTPHandler serves srv over streamable HTTP. With a token, every request
// needs "Authorization: Bearer <token>". Cross-origin browser requests are
// rejected before anything else runs.
func HTTPHandler(srv *mcp.Server, token string) http.Handler {
	var h http.Handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	if token != "" {
		h = requireToken(token, h)
	}
	return http.NewCrossOriginProtection().Handler(h)
}

func requireToken(token string, h http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// NewHTTPServer returns a server for h with header and idle timeouts. It sets
// no read or write timeout, because the SSE stream of a session is
// long-lived.
func NewHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}
