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
