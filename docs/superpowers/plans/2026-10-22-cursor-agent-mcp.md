# Cursor Agent MCP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Dockerized MCP server (Streamable HTTP) exposing 12 Cursor Cloud Agents API tools, with the Cursor API key injected per request or via env.

**Architecture:** One Go `main` package. `handler()` wraps a single `*mcp.Server` in a stateless `StreamableHTTPHandler`. Every tool goes through `addTool`, which resolves the key, turns typed input into an `endpoint`, calls Cursor via `call`, and returns the raw JSON as text. Errors become `IsError` tool results.

**Tech Stack:** Go 1.25, `github.com/modelcontextprotocol/go-sdk` v1.8.0, stdlib `net/http`/`httptest`, `gcr.io/distroless/static:nonroot`.

**Spec:** `docs/superpowers/specs/2026-10-22-cursor-agent-mcp-design.md`

## Global Constraints

- No local Go toolchain: run all Go commands through `docker run --rm -v "$PWD":/src -w /src -v cursor-mcp-gomod:/go/pkg/mod golang:1.25-alpine <cmd>` (call it `$GO` below; define it with `GO='docker run --rm -v "$PWD":/src -w /src -v cursor-mcp-gomod:/go/pkg/mod golang:1.25-alpine'`).
- Module path `cursor-agent-mcp`; files live at repo root: `go.mod`, `go.sum`, `main.go`, `main_test.go`, `Dockerfile`, `README.md`.
- Cursor base URL `https://api.cursor.com`, auth `Authorization: Bearer <key>`.
- Key order: MCP request `Authorization: Bearer` header → `CURSOR_API_KEY` env → tool error `no Cursor API key: set CURSOR_API_KEY or send Authorization: Bearer <key>`. Never log the key.
- Listen on `:8080`, path `/mcp`, `Stateless: true`.
- `gofmt -l .` must print nothing.

## Review Focus

1. Empty, `.`, `..` or slash-containing `agent_id`/`run_id` — must not hit a different endpoint (`get_agent("")` would be `list_agents`; `delete_agent("..")` would hit `/v1`). Pinned in Task 2 `TestRejectsBadInput` and the `a/b?c` route case.
2. `pr_url` without `repo_url` — Cursor requires `url` on every repo entry; reject before calling. Pinned in Task 2 `TestRejectsBadInput`.
3. `Authorization` header that's lowercase `bearer` or not Bearer at all (e.g. `Basic`) — lowercase works, non-Bearer falls back to env. Pinned in Task 1 `TestAPIKeyResolution`.
4. `list_repositories` taking tens of seconds — 2 minute client timeout; not unit tested (would need a slow fake), checked by reading `httpClient`.
5. Container port reachable from the LAN in env-key mode — README and smoke run bind `127.0.0.1:`; checked in Task 3 Step 3.

---

### Task 1: Server, key resolution, Cursor client, `me` tool

**Files:**
- Create: `go.mod`, `go.sum`, `main.go`, `main_test.go`

**Interfaces:**
- Produces: `cursorBaseURL string` (var), `handler() http.Handler`, `type endpoint struct{method, path string; query url.Values; body any}`, `apiKey(*mcp.CallToolRequest) string`, `call(ctx, key string, e endpoint) (string, error)`, `addTool[In any](s *mcp.Server, t *mcp.Tool, route func(In) (endpoint, error))`, `type noInput struct{}`, `readOnly *mcp.ToolAnnotations`, `newServer() *mcp.Server`. Test helpers: `fakeCursor(t, status, resp) *seen`, `callTool(t, auth, name, args) *mcp.CallToolResult`, `text(*mcp.CallToolResult) string`.

- [ ] **Step 1: Init module**

```bash
$GO sh -c 'go mod init cursor-agent-mcp && go get github.com/modelcontextprotocol/go-sdk@v1.8.0'
```

- [ ] **Step 2: Write the failing tests** — `main_test.go`:

```go
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
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `$GO go test ./...`
Expected: FAIL to compile — `undefined: cursorBaseURL`, `undefined: handler`.

- [ ] **Step 4: Write minimal implementation** — `main.go`:

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// cursorBaseURL is a var so tests can point it at a fake Cursor API.
var cursorBaseURL = "https://api.cursor.com"

// list_repositories can take tens of seconds, so the timeout is generous.
var httpClient = &http.Client{Timeout: 2 * time.Minute}

var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true}

func main() {
	http.Handle("/mcp", handler())
	log.Println("cursor-agent-mcp listening on :8080/mcp")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handler() http.Handler {
	s := newServer()
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true})
}

type endpoint struct {
	method, path string
	query        url.Values
	body         any
}

// apiKey prefers the MCP request's "Authorization: Bearer <key>" header,
// falling back to the CURSOR_API_KEY env var.
func apiKey(req *mcp.CallToolRequest) string {
	if req.Extra != nil {
		auth := req.Extra.Header.Get("Authorization")
		if len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") {
			return strings.TrimSpace(auth[7:])
		}
	}
	return os.Getenv("CURSOR_API_KEY")
}

func call(ctx context.Context, key string, e endpoint) (string, error) {
	var body io.Reader
	if e.body != nil {
		b, err := json.Marshal(e.body)
		if err != nil {
			return "", err
		}
		body = bytes.NewReader(b)
	}
	u := cursorBaseURL + e.path
	if len(e.query) > 0 {
		u += "?" + e.query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, e.method, u, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	if res.StatusCode/100 != 2 {
		return "", fmt.Errorf("cursor %d: %s", res.StatusCode, b)
	}
	return string(b), nil
}

// addTool registers a tool that maps its input to one Cursor API call and
// returns Cursor's JSON response as text. Errors become IsError results.
func addTool[In any](s *mcp.Server, t *mcp.Tool, route func(In) (endpoint, error)) {
	mcp.AddTool(s, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		key := apiKey(req)
		if key == "" {
			return nil, nil, errors.New("no Cursor API key: set CURSOR_API_KEY or send Authorization: Bearer <key>")
		}
		e, err := route(in)
		if err != nil {
			return nil, nil, err
		}
		text, err := call(ctx, key, e)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	})
}

type noInput struct{}

func newServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "cursor-agent-mcp", Version: "0.1.0"}, nil)

	addTool(s, &mcp.Tool{Name: "me", Description: "Show info about the Cursor API key in use.", Annotations: readOnly},
		func(noInput) (endpoint, error) { return endpoint{method: "GET", path: "/v1/me"}, nil })

	return s
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `$GO sh -c 'go mod tidy && gofmt -l . && go vet ./... && go test ./...'`
Expected: no gofmt output, `ok  cursor-agent-mcp`.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum main.go main_test.go
git commit -m "Add MCP server with Cursor key resolution and me tool"
```

### Task 2: Agent lifecycle tools

**Files:**
- Modify: `main.go` (add `"strconv"` import, input types + helpers before `newServer`, tools inside `newServer` after `me`)
- Modify: `main_test.go` (add `"encoding/json"`, `"reflect"` imports; append tests)

**Interfaces:**
- Consumes: `addTool`, `endpoint`, `noInput`, `readOnly`, `fakeCursor`, `callTool`, `text` from Task 1.
- Produces: `agentPath(segs ...string) (string, error)`, `pageQuery(limit int, cursor string) url.Values`; tools `create_agent`, `list_agents`, `get_agent`, `followup`, `list_runs`, `get_run`, `cancel_run`, `archive_agent`, `delete_agent`, `list_models`, `list_repositories`.

- [ ] **Step 1: Write the failing tests** — add `"encoding/json"` and `"reflect"` to `main_test.go` imports, then append:

```go
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
		{"list_agents", map[string]any{"limit": 5, "cursor": "c1", "pr_url": "https://github.com/o/r/pull/1", "include_archived": false},
			"GET", "/v1/agents", "cursor=c1&includeArchived=false&limit=5&prUrl=https%3A%2F%2Fgithub.com%2Fo%2Fr%2Fpull%2F1", ""},
		{"list_agents", nil, "GET", "/v1/agents", "", ""},
		{"get_agent", map[string]any{"agent_id": "bc-1"}, "GET", "/v1/agents/bc-1", "", ""},
		{"get_agent", map[string]any{"agent_id": "a/b?c"}, "GET", "/v1/agents/a%2Fb%3Fc", "", ""},
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
		{"get_run", map[string]any{"agent_id": "bc-1", "run_id": ""}, "invalid id"},
		{"create_agent", map[string]any{"prompt": "x", "pr_url": "https://github.com/o/r/pull/1"}, "pr_url requires repo_url"},
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `$GO go test ./...`
Expected: FAIL — `TestToolRoutes/create_agent` etc. report `unknown tool` errors (from `t.Fatal` in `callTool` or `IsError`), `TestRejectsBadInput` fails.

- [ ] **Step 3: Implement** — add `"strconv"` to `main.go` imports; insert before `func newServer()`:

```go
type agentRef struct {
	AgentID string `json:"agent_id" jsonschema:"agent id, e.g. bc-..."`
}

type runRef struct {
	AgentID string `json:"agent_id" jsonschema:"agent id, e.g. bc-..."`
	RunID   string `json:"run_id"`
}

type createAgentIn struct {
	Prompt       string `json:"prompt" jsonschema:"task for the agent"`
	RepoURL      string `json:"repo_url,omitempty" jsonschema:"repository URL, e.g. https://github.com/org/repo; omit for a no-repo agent"`
	StartingRef  string `json:"starting_ref,omitempty" jsonschema:"branch or commit SHA to start from"`
	PrURL        string `json:"pr_url,omitempty" jsonschema:"existing PR to work on; requires repo_url and overrides starting_ref"`
	Model        string `json:"model,omitempty" jsonschema:"model id from list_models; omit for the default"`
	Name         string `json:"name,omitempty" jsonschema:"display name, max 100 chars"`
	Mode         string `json:"mode,omitempty" jsonschema:"agent (default) or plan"`
	AutoCreatePR bool   `json:"auto_create_pr,omitempty" jsonschema:"open a PR when the run completes"`
}

type listAgentsIn struct {
	Limit           int    `json:"limit,omitempty" jsonschema:"page size, default 20, max 100"`
	Cursor          string `json:"cursor,omitempty" jsonschema:"nextCursor from the previous page"`
	PrURL           string `json:"pr_url,omitempty" jsonschema:"only agents working on this PR"`
	IncludeArchived *bool  `json:"include_archived,omitempty" jsonschema:"default true"`
}

type followupIn struct {
	AgentID string `json:"agent_id" jsonschema:"agent id, e.g. bc-..."`
	Prompt  string `json:"prompt" jsonschema:"follow-up instruction"`
	Mode    string `json:"mode,omitempty" jsonschema:"agent (default) or plan"`
}

type listRunsIn struct {
	AgentID string `json:"agent_id" jsonschema:"agent id, e.g. bc-..."`
	Limit   int    `json:"limit,omitempty" jsonschema:"page size, default 20, max 100"`
	Cursor  string `json:"cursor,omitempty" jsonschema:"nextCursor from the previous page"`
}

// agentPath builds /v1/agents/{segs...} with each segment escaped. Empty and
// dot segments are rejected so get_agent("") can't turn into list_agents.
func agentPath(segs ...string) (string, error) {
	p := "/v1/agents"
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("invalid id %q", s)
		}
		p += "/" + url.PathEscape(s)
	}
	return p, nil
}

func pageQuery(limit int, cursor string) url.Values {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	return q
}

```

Then inside `newServer`, after the `me` tool and before `return s`:

```go
	addTool(s, &mcp.Tool{Name: "create_agent", Description: "Start a Cursor cloud agent, optionally on a repo. Returns the agent and its first run; poll get_run for status, result, branch and PR."},
		func(in createAgentIn) (endpoint, error) {
			if in.PrURL != "" && in.RepoURL == "" {
				return endpoint{}, errors.New("pr_url requires repo_url")
			}
			body := map[string]any{"prompt": map[string]string{"text": in.Prompt}}
			if in.RepoURL != "" {
				repo := map[string]string{"url": in.RepoURL}
				if in.StartingRef != "" {
					repo["startingRef"] = in.StartingRef
				}
				if in.PrURL != "" {
					repo["prUrl"] = in.PrURL
				}
				body["repos"] = []map[string]string{repo}
			}
			if in.Model != "" {
				body["model"] = map[string]string{"id": in.Model}
			}
			if in.Name != "" {
				body["name"] = in.Name
			}
			if in.Mode != "" {
				body["mode"] = in.Mode
			}
			if in.AutoCreatePR {
				body["autoCreatePR"] = true
			}
			return endpoint{method: "POST", path: "/v1/agents", body: body}, nil
		})

	addTool(s, &mcp.Tool{Name: "list_agents", Description: "List agents, newest first. Pass nextCursor back as cursor for the next page.", Annotations: readOnly},
		func(in listAgentsIn) (endpoint, error) {
			q := pageQuery(in.Limit, in.Cursor)
			if in.PrURL != "" {
				q.Set("prUrl", in.PrURL)
			}
			if in.IncludeArchived != nil {
				q.Set("includeArchived", strconv.FormatBool(*in.IncludeArchived))
			}
			return endpoint{method: "GET", path: "/v1/agents", query: q}, nil
		})

	addTool(s, &mcp.Tool{Name: "get_agent", Description: "Get an agent's metadata (status ACTIVE, IDLE or ARCHIVED).", Annotations: readOnly},
		func(in agentRef) (endpoint, error) {
			p, err := agentPath(in.AgentID)
			return endpoint{method: "GET", path: p}, err
		})

	addTool(s, &mcp.Tool{Name: "followup", Description: "Send a follow-up prompt to an agent, starting a new run. Fails with agent_busy while a run is active."},
		func(in followupIn) (endpoint, error) {
			p, err := agentPath(in.AgentID, "runs")
			body := map[string]any{"prompt": map[string]string{"text": in.Prompt}}
			if in.Mode != "" {
				body["mode"] = in.Mode
			}
			return endpoint{method: "POST", path: p, body: body}, err
		})

	addTool(s, &mcp.Tool{Name: "list_runs", Description: "List an agent's runs.", Annotations: readOnly},
		func(in listRunsIn) (endpoint, error) {
			p, err := agentPath(in.AgentID, "runs")
			return endpoint{method: "GET", path: p, query: pageQuery(in.Limit, in.Cursor)}, err
		})

	addTool(s, &mcp.Tool{Name: "get_run", Description: "Get a run's status; finished runs include result, durationMs and git branches/PR URLs.", Annotations: readOnly},
		func(in runRef) (endpoint, error) {
			p, err := agentPath(in.AgentID, "runs", in.RunID)
			return endpoint{method: "GET", path: p}, err
		})

	addTool(s, &mcp.Tool{Name: "cancel_run", Description: "Cancel an active run. Cancellation is permanent."},
		func(in runRef) (endpoint, error) {
			p, err := agentPath(in.AgentID, "runs", in.RunID, "cancel")
			return endpoint{method: "POST", path: p}, err
		})

	addTool(s, &mcp.Tool{Name: "archive_agent", Description: "Archive an agent. Reversible from the Cursor dashboard or API."},
		func(in agentRef) (endpoint, error) {
			p, err := agentPath(in.AgentID, "archive")
			return endpoint{method: "POST", path: p}, err
		})

	addTool(s, &mcp.Tool{Name: "delete_agent", Description: "Permanently delete an agent. Cannot be undone."},
		func(in agentRef) (endpoint, error) {
			p, err := agentPath(in.AgentID)
			return endpoint{method: "DELETE", path: p}, err
		})

	addTool(s, &mcp.Tool{Name: "list_models", Description: "List model ids usable as create_agent's model.", Annotations: readOnly},
		func(noInput) (endpoint, error) { return endpoint{method: "GET", path: "/v1/models"}, nil })

	addTool(s, &mcp.Tool{Name: "list_repositories", Description: "List GitHub repos Cursor can access. Slow (tens of seconds) and rate limited to 1 call per minute.", Annotations: readOnly},
		func(noInput) (endpoint, error) { return endpoint{method: "GET", path: "/v1/repositories"}, nil })

```

- [ ] **Step 4: Run tests to verify they pass**

Run: `$GO sh -c 'gofmt -l . && go vet ./... && go test ./...'`
Expected: no gofmt output, `ok  cursor-agent-mcp`.

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "Add Cursor agent lifecycle tools"
```

### Task 3: Docker image, README, smoke run

**Files:**
- Create: `Dockerfile`, `.dockerignore`, `README.md`

**Interfaces:**
- Consumes: `main` package from Tasks 1–2.

- [ ] **Step 1: `Dockerfile`**

```dockerfile
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN go test ./... && CGO_ENABLED=0 go build -o /cursor-agent-mcp .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /cursor-agent-mcp /cursor-agent-mcp
EXPOSE 8080
ENTRYPOINT ["/cursor-agent-mcp"]
```

`.dockerignore`:

```
.git
.context
docs
```

- [ ] **Step 2: `README.md`**

```markdown
# cursor-agent-mcp

MCP server for [Cursor Cloud Agents](https://cursor.com/docs/cloud-agent/api/endpoints), served over Streamable HTTP from Docker.

## Run

```sh
docker build -t cursor-agent-mcp .
docker run --rm -p 127.0.0.1:8080:8080 -e CURSOR_API_KEY cursor-agent-mcp
```

Keep the `127.0.0.1:` prefix: with `CURSOR_API_KEY` set, anyone who can reach the port uses your key.

## API key

Each tool call uses, in order:

1. `Authorization: Bearer <key>` on the MCP request
2. `CURSOR_API_KEY` in the container env

Leave `-e CURSOR_API_KEY` off to require the header on every request (one container, many keys).

## Connect

Claude Code:

```sh
claude mcp add --transport http cursor http://localhost:8080/mcp
# or pass the key per client instead of via the container env:
claude mcp add --transport http cursor http://localhost:8080/mcp --header "Authorization: Bearer $CURSOR_API_KEY"
```

Other clients (`mcp.json`):

```json
{
  "mcpServers": {
    "cursor": {
      "url": "http://localhost:8080/mcp",
      "headers": { "Authorization": "Bearer <cursor api key>" }
    }
  }
}
```

## Tools

`create_agent`, `list_agents`, `get_agent`, `followup`, `list_runs`, `get_run`, `cancel_run`, `archive_agent`, `delete_agent`, `list_models`, `list_repositories`, `me`. Each maps to one `/v1` endpoint and returns Cursor's JSON as-is. Poll `get_run` to follow a run.

## Develop

No local Go needed:

```sh
docker run --rm -v "$PWD":/src -w /src -v cursor-mcp-gomod:/go/pkg/mod golang:1.25-alpine go test ./...
```

`docker build` also runs the tests.
```

- [ ] **Step 3: Build and smoke test against the real API with a fake key**

```bash
docker build -t cursor-agent-mcp .
docker run -d --rm --name cursor-mcp-smoke -p 127.0.0.1:18080:8080 -e CURSOR_API_KEY=fake cursor-agent-mcp
sleep 1
H=(-H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream')
curl -s localhost:18080/mcp "${H[@]}" -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | grep -o '"name":"[a-z_]*"' | wc -l
curl -s localhost:18080/mcp "${H[@]}" -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"me","arguments":{}}}'
docker port cursor-mcp-smoke
docker stop cursor-mcp-smoke
```

Expected: build runs tests and succeeds; image ≈14 MB; `12`; the `me` call returns `"isError":true` with `cursor 401` from api.cursor.com; `docker port` shows `127.0.0.1:18080`.

- [ ] **Step 4: Commit**

```bash
git add Dockerfile .dockerignore README.md
git commit -m "Add Dockerfile and README"
```

## Skipped

- Multi-repo create, images, mcpServers/envVars/subagents on create — add when needed from chat.
- SSE `/stream` and a `wait_for_run` tool — add if polling `get_run` is annoying.
- usage, artifacts, unarchive, environments, secrets, workers — add when a workflow needs them.
- stdio entrypoint — add if a client can't do HTTP.
- docker compose file — one `docker run` line covers it.
