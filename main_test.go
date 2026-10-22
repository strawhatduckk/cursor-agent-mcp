package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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

// connect opens an MCP client session against the real HTTP handler.
func connect(t *testing.T, auth string) *mcp.ClientSession {
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
	t.Cleanup(func() { session.Close() })
	return session
}

// callTool calls a tool through the real HTTP handler with an MCP client.
func callTool(t *testing.T, auth, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := connect(t, auth).CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
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

func TestToolRoutes(t *testing.T) {
	cases := []struct {
		tool                string
		args                map[string]any
		method, path, query string
		body                string // JSON, compared structurally; "" means no body
	}{
		{"create_agent", map[string]any{"prompt": "fix bug", "repo_url": "https://github.com/o/r", "starting_ref": "main", "model": "gpt-5", "name": "n", "mode": "plan", "auto_create_pr": true},
			"POST", "/v1/agents", "",
			`{"prompt":{"text":"fix bug"},"repos":[{"url":"https://github.com/o/r","startingRef":"main"}],"model":{"id":"gpt-5"},"name":"n","mode":"plan","autoCreatePR":true}`},
		{"create_agent", map[string]any{"prompt": "review", "repo_url": "https://github.com/o/r", "pr_url": "https://github.com/o/r/pull/1"},
			"POST", "/v1/agents", "",
			`{"prompt":{"text":"review"},"repos":[{"url":"https://github.com/o/r","prUrl":"https://github.com/o/r/pull/1"}]}`},
		{"create_agent", map[string]any{"prompt": "hi"}, "POST", "/v1/agents", "", `{"prompt":{"text":"hi"}}`},
		{"create_agent", map[string]any{"prompt": "hi", "model": "claude-opus-5-5", "model_params": map[string]any{"effort": "high", "context": "200k"}},
			"POST", "/v1/agents", "",
			`{"prompt":{"text":"hi"},"model":{"id":"claude-opus-5-5","params":[{"id":"context","value":"200k"},{"id":"effort","value":"high"}]}}`},
		{"list_agents", map[string]any{"limit": 5, "cursor": "c1", "pr_url": "https://github.com/o/r/pull/1", "include_archived": false},
			"GET", "/v1/agents", "cursor=c1&includeArchived=false&limit=5&prUrl=https%3A%2F%2Fgithub.com%2Fo%2Fr%2Fpull%2F1", ""},
		{"list_agents", nil, "GET", "/v1/agents", "", ""},
		{"get_agent", map[string]any{"agent_id": "bc-1"}, "GET", "/v1/agents/bc-1", "", ""},
		{"followup", map[string]any{"agent_id": "bc-1", "prompt": "more", "mode": "plan"},
			"POST", "/v1/agents/bc-1/runs", "", `{"prompt":{"text":"more"},"mode":"plan"}`},
		{"list_runs", map[string]any{"agent_id": "bc-1", "limit": 2}, "GET", "/v1/agents/bc-1/runs", "limit=2", ""},
		{"get_run", map[string]any{"agent_id": "bc-1", "run_id": "run-1"}, "GET", "/v1/agents/bc-1/runs/run-1", "", ""},
		{"cancel_run", map[string]any{"agent_id": "bc-1", "run_id": "run-1"}, "POST", "/v1/agents/bc-1/runs/run-1/cancel", "", ""},
		{"archive_agent", map[string]any{"agent_id": "bc-1"}, "POST", "/v1/agents/bc-1/archive", "", ""},
		{"delete_agent", map[string]any{"agent_id": "bc-1"}, "DELETE", "/v1/agents/bc-1", "", ""},
		{"list_models", nil, "GET", "/v1/models", "", ""},
		{"list_repositories", nil, "GET", "/v1/repositories", "", ""},
		{"me", nil, "GET", "/v1/me", "", ""},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			t.Setenv("CURSOR_API_KEY", "k")
			got := fakeCursor(t, 200, `{"ok":true}`)
			res := callTool(t, "", c.tool, c.args)
			if res.IsError {
				t.Fatalf("unexpected error: %s", text(res))
			}
			if got.method != c.method || got.path != c.path || got.query != c.query {
				t.Errorf("got %s %s?%s, want %s %s?%s", got.method, got.path, got.query, c.method, c.path, c.query)
			}
			if !jsonEqual(got.body, c.body) {
				t.Errorf("body = %s, want %s", got.body, c.body)
			}
		})
	}
}

func TestRejectsBadInput(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"get_agent", map[string]any{"agent_id": ""}, "invalid id"},
		{"delete_agent", map[string]any{"agent_id": ".."}, "invalid id"},
		{"delete_agent", map[string]any{"agent_id": "a/../b"}, "invalid id"},
		{"get_agent", map[string]any{"agent_id": "a%2Fb"}, "invalid id"},
		{"cancel_run", map[string]any{"agent_id": "bc-1", "run_id": "r?x#y"}, "invalid id"},
		{"get_run", map[string]any{"agent_id": "bc-1", "run_id": ""}, "invalid id"},
		{"create_agent", map[string]any{"prompt": "x", "pr_url": "https://github.com/o/r/pull/1"}, "pr_url requires repo_url"},
		{"create_agent", map[string]any{"prompt": "x", "model_params": map[string]any{"effort": "high"}}, "model_params requires model"},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			t.Setenv("CURSOR_API_KEY", "k")
			got := fakeCursor(t, 200, `{}`)
			res := callTool(t, "", c.tool, c.args)
			if !res.IsError || !strings.Contains(text(res), c.want) {
				t.Fatalf("want error containing %q, got IsError=%v %q", c.want, res.IsError, text(res))
			}
			if got.method != "" {
				t.Errorf("Cursor was called: %s %s", got.method, got.path)
			}
		})
	}
}

func jsonEqual(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	var x, y any
	return json.Unmarshal([]byte(a), &x) == nil && json.Unmarshal([]byte(b), &y) == nil && reflect.DeepEqual(x, y)
}

// DNS rebinding: inside Docker the SDK's loopback-only Host check never runs,
// so the handler itself must refuse non-local Host headers.
func TestRejectsNonLocalHost(t *testing.T) {
	for _, host := range []string{"evil.example:8080", "evil.example", "192.168.1.5:8080"} {
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
		req.Host = host
		rec := httptest.NewRecorder()
		handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Host %q: status %d, want 403", host, rec.Code)
		}
	}
}

// Clients warn on destructive tools, so only delete_agent and cancel_run
// (both irreversible) should keep the MCP default destructiveHint=true.
func TestDestructiveHints(t *testing.T) {
	res, err := connect(t, "").ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"create_agent": false, "followup": false, "archive_agent": false, "delete_agent": true, "cancel_run": true}
	for _, tool := range res.Tools {
		w, ok := want[tool.Name]
		if !ok {
			continue
		}
		got := tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint
		if got != w {
			t.Errorf("%s destructive = %v, want %v", tool.Name, got, w)
		}
	}
}

// The README tells clients to use localhost, so the allowlist must let the
// local names through (anything but 403; the SDK may still reject the body).
func TestAllowsLocalHost(t *testing.T) {
	for _, host := range []string{"localhost:8080", "127.0.0.1:8080", "[::1]:8080"} {
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
		req.Host = host
		rec := httptest.NewRecorder()
		handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden {
			t.Errorf("Host %q: got 403, want allowed", host)
		}
	}
}
