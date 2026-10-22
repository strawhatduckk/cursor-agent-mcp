package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
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

// handler refuses non-local Host headers to block DNS rebinding. The SDK's own
// check only runs when the connection arrives on loopback, which it never
// does inside Docker.
func handler() http.Handler {
	s := newServer()
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
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

// agentPath builds /v1/agents/{segs...}. Empty, dot and path/URL-separator
// segments are rejected so an id can never reach a different endpoint (e.g.
// get_agent("") turning into list_agents). Real ids are bc-<uuid>/run-<uuid>.
func agentPath(segs ...string) (string, error) {
	p := "/v1/agents"
	for _, s := range segs {
		if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\\?#%") {
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

func newServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "cursor-agent-mcp", Version: "0.1.0"}, nil)

	addTool(s, &mcp.Tool{Name: "me", Description: "Show info about the Cursor API key in use.", Annotations: readOnly},
		func(noInput) (endpoint, error) { return endpoint{method: "GET", path: "/v1/me"}, nil })

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

	return s
}
