package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type seen struct{ method, path, query, auth, body string }

// fakeCursor points the server at a fake Cursor API that answers every
// request with status and resp, and records the last request it got.
func fakeCursor(t *testing.T, status int, resp string) *seen {
	t.Helper()
	got := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = seen{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Authorization"), string(b)}
		w.WriteHeader(status)
		io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	old := cursorBaseURL
	cursorBaseURL = srv.URL
	t.Cleanup(func() { cursorBaseURL = old })
	return got
}

// withAuth sets the Authorization header on every MCP request, like a
// client configured with --header.
type withAuth string

func (a withAuth) RoundTrip(r *http.Request) (*http.Response, error) {
	if a != "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", string(a))
	}
	return http.DefaultTransport.RoundTrip(r)
}

// callTool calls a tool through the real HTTP handler with an MCP client.
func callTool(t *testing.T, auth, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	srv := httptest.NewServer(handler())
	t.Cleanup(srv.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             srv.URL,
		HTTPClient:           &http.Client{Transport: withAuth(auth)},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func text(r *mcp.CallToolResult) string { return r.Content[0].(*mcp.TextContent).Text }

func TestAPIKeyResolution(t *testing.T) {
	cases := []struct{ name, header, env, wantAuth string }{
		{"header wins over env", "Bearer header-key", "env-key", "Bearer header-key"},
		{"lowercase bearer", "bearer header-key", "env-key", "Bearer header-key"},
		{"env when no header", "", "env-key", "Bearer env-key"},
		{"env when header is not bearer", "Basic abc", "env-key", "Bearer env-key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CURSOR_API_KEY", c.env)
			got := fakeCursor(t, 200, `{"apiKeyName":"k"}`)
			res := callTool(t, c.header, "me", nil)
			if res.IsError {
				t.Fatalf("unexpected error: %s", text(res))
			}
			if got.auth != c.wantAuth {
				t.Errorf("auth = %q, want %q", got.auth, c.wantAuth)
			}
			if text(res) != `{"apiKeyName":"k"}` {
				t.Errorf("text = %q", text(res))
			}
		})
	}
}

func TestMissingKeyIsToolError(t *testing.T) {
	t.Setenv("CURSOR_API_KEY", "")
	got := fakeCursor(t, 200, `{}`)
	res := callTool(t, "", "me", nil)
	if !res.IsError || !strings.Contains(text(res), "no Cursor API key") {
		t.Fatalf("want missing-key error, got IsError=%v %q", res.IsError, text(res))
	}
	if got.method != "" {
		t.Errorf("Cursor was called without a key")
	}
}

func TestCursorErrorIsToolError(t *testing.T) {
	t.Setenv("CURSOR_API_KEY", "k")
	fakeCursor(t, 409, `{"error":"agent_busy"}`)
	res := callTool(t, "", "me", nil)
	if !res.IsError || !strings.Contains(text(res), "409") || !strings.Contains(text(res), "agent_busy") {
		t.Fatalf("want 409 agent_busy tool error, got IsError=%v %q", res.IsError, text(res))
	}
}
