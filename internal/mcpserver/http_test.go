package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCheckListenAddr(t *testing.T) {
	const good = "0123456789abcdef"
	cases := []struct {
		addr   string
		remote bool
		token  string
		ok     bool
	}{
		{"127.0.0.1:7077", false, "", true},
		{"[::1]:7077", false, "", true},
		{"localhost:7077", false, "", true},
		{"127.0.0.1:7077", false, good, true},
		{"127.0.0.1:7077", false, "short", false},
		{"0.0.0.0:7077", false, "", false},
		{":7077", false, "", false},
		{"[::]:7077", false, good, false},
		{"192.168.1.5:7077", false, good, false},
		{"0.0.0.0:7077", true, "", false},
		{"0.0.0.0:7077", true, "short", false},
		{"0.0.0.0:7077", true, good, true},
		{":7077", true, good, true},
		{"no-port", false, "", false},
	}
	for _, c := range cases {
		err := CheckListenAddr(c.addr, c.remote, c.token)
		if (err == nil) != c.ok {
			t.Errorf("CheckListenAddr(%q, %v, %q) = %v, want ok=%v", c.addr, c.remote, c.token, err, c.ok)
		}
	}
	if err := CheckListenAddr("0.0.0.0:7077", false, ""); err == nil || !strings.Contains(err.Error(), "pass -allow-remote and set SCREE_HTTP_TOKEN") {
		t.Errorf("refusal = %v", err)
	}
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

func post(t *testing.T, url string, header map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(initBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func testServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "scree-test"}, nil)
	ts := httptest.NewServer(HTTPHandler(srv, token))
	t.Cleanup(ts.Close)
	return ts
}

func TestHTTPRequiresToken(t *testing.T) {
	const token = "0123456789abcdef-token"
	ts := testServer(t, token)
	for name, h := range map[string]map[string]string{
		"no header":   nil,
		"wrong token": {"Authorization": "Bearer nope-nope-nope-nope"},
		"no scheme":   {"Authorization": token},
	} {
		resp := post(t, ts.URL, h)
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: status %d, WWW-Authenticate %q", name, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}
	if resp := post(t, ts.URL, map[string]string{"Authorization": "Bearer " + token}); resp.StatusCode != http.StatusOK {
		t.Errorf("right token: status %d", resp.StatusCode)
	}
	// Without a token configured, loopback clients need no header.
	if resp := post(t, testServer(t, "").URL, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("no token configured: status %d", resp.StatusCode)
	}
}

func TestHTTPRejectsCrossOrigin(t *testing.T) {
	const token = "0123456789abcdef-token"
	ts := testServer(t, token)
	resp := post(t, ts.URL, map[string]string{"Authorization": "Bearer " + token, "Sec-Fetch-Site": "cross-site"})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site POST: status %d, want 403", resp.StatusCode)
	}
	resp = post(t, ts.URL, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site POST without token: status %d, want 403 before auth", resp.StatusCode)
	}
}

func TestNewHTTPServerTimeouts(t *testing.T) {
	s := NewHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	if s.ReadHeaderTimeout == 0 || s.IdleTimeout == 0 || s.MaxHeaderBytes == 0 {
		t.Fatalf("server = %+v", s)
	}
	if s.ReadTimeout != 0 || s.WriteTimeout != 0 {
		t.Error("read or write timeout would cut the SSE stream")
	}
}
