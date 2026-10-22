# Cursor Agent MCP — Design

## Goal

An MCP server, run locally in Docker, that lets an MCP client (e.g. Claude Code) start and manage Cursor Cloud Agents via the API at https://cursor.com/docs/cloud-agent/api/endpoints. The Cursor API key is injected at runtime, never baked into the image.

## Decisions

- **Transport:** Streamable HTTP, stateless, at `http://localhost:8080/mcp`.
- **Scope:** agent lifecycle only (12 tools below).
- **Stack:** Go + official `github.com/modelcontextprotocol/go-sdk` (v1.8.0). Built and tested inside Docker; no local Go needed.

## Runtime

- `mcp.NewStreamableHTTPHandler(getServer, &mcp.StreamableHTTPOptions{Stateless: true})` mounted at `/mcp`, listening on `:8080`.
- Run: `docker run --rm -p 127.0.0.1:8080:8080 -e CURSOR_API_KEY cursor-agent-mcp`. Publish on 127.0.0.1 only: in env-key mode anyone reaching the port uses the key.
- Host header must be `localhost`, `127.0.0.1` or `::1` (403 otherwise). The SDK's DNS-rebinding check only runs on loopback connections, which never happen inside Docker.
- Client: `claude mcp add --transport http cursor http://localhost:8080/mcp [--header "Authorization: Bearer <key>"]`.

## API key resolution

Per tool call, in order:

1. `Authorization: Bearer <key>` header on the MCP HTTP request (`req.Extra.Header`).
2. `CURSOR_API_KEY` env var.
3. Neither → tool error `no Cursor API key: set CURSOR_API_KEY or send Authorization: Bearer <key>`.

The key is never logged.

## Cursor client

One function: `call(ctx, key string, e endpoint) (string, error)` where `endpoint` is `{method, path, query url.Values, body any}`.

- `net/http`, 2 minute timeout (`list_repositories` can take tens of seconds), `Authorization: Bearer <key>`, JSON body when `body != nil`.
- Base URL `https://api.cursor.com` in a package var (tests override it).
- 2xx → raw response body text. Non-2xx → error `cursor <status>: <body>` so the model sees codes like `agent_busy`, `run_not_cancellable`.
- Tool handlers turn errors into `CallToolResult{IsError: true}` with the error text.

## Tools

Typed input structs (schema inferred by the SDK); output is Cursor's JSON passed through as text content. GET tools carry `ReadOnlyHint`. `create_agent`, `followup` and `archive_agent` set `DestructiveHint: false` (additive or reversible); `cancel_run` and `delete_agent` keep the MCP default (destructive).

| Tool | Endpoint | Inputs (* required) |
|---|---|---|
| `create_agent` | POST /v1/agents | `prompt`*, `repo_url`, `starting_ref`, `pr_url`, `model`, `model_params` (map → `model.params[{id,value}]`, requires `model`), `name`, `mode` (agent\|plan), `auto_create_pr` |
| `list_agents` | GET /v1/agents | `limit`, `cursor`, `pr_url`, `include_archived` |
| `get_agent` | GET /v1/agents/{id} | `agent_id`* |
| `followup` | POST /v1/agents/{id}/runs | `agent_id`*, `prompt`*, `mode` |
| `list_runs` | GET /v1/agents/{id}/runs | `agent_id`*, `limit`, `cursor` |
| `get_run` | GET /v1/agents/{id}/runs/{runId} | `agent_id`*, `run_id`* |
| `cancel_run` | POST /v1/agents/{id}/runs/{runId}/cancel | `agent_id`*, `run_id`* |
| `archive_agent` | POST /v1/agents/{id}/archive | `agent_id`* |
| `delete_agent` | DELETE /v1/agents/{id} | `agent_id`* |
| `list_models` | GET /v1/models | — |
| `list_repositories` | GET /v1/repositories | — |
| `me` | GET /v1/me | — |

Body mapping for `create_agent`: `{"prompt":{"text":prompt}, "repos":[{"url","startingRef","prUrl"}] (only when repo_url set; pr_url or starting_ref without repo_url is rejected rather than dropped, since Cursor requires url on every repo entry), "model":{"id":model,"params":[{"id","value"}...]} (when set; params from model_params sorted by id; model_params without model is rejected), "name", "mode", "autoCreatePR"}` — omit empty fields. Path segments built with `url.PathEscape`; empty, `.`, `..` ids and ids containing `/ \ ? # %` are rejected before calling Cursor, so an id can never reach a different endpoint (an empty `agent_id` would otherwise turn `get_agent` into `list_agents`).

## Docker

Multi-stage: `golang:1.25-alpine` runs `go test ./...` then `CGO_ENABLED=0 go build`; final stage `gcr.io/distroless/static:nonroot`, `EXPOSE 8080`.

## Testing

`main_test.go` with an `httptest` fake Cursor server and the SDK's streamable client transport against an `httptest` MCP server:

- header key wins over env key
- env key used when no header
- missing key → `IsError`
- `create_agent` sends correct method, path, body
- Cursor 409 → `IsError` containing the response body

## Ponytail

- Reused: none (empty repo)
- Built with: HTTP transport + schemas → go-sdk; Cursor client → net/http + encoding/json (stdlib); image → distroless; tests → testing + httptest (stdlib)
- Skipped: multi-repo create, images, mcpServers/envVars/subagents on create — add when needed from chat; SSE stream / `wait_for_run` — add if polling `get_run` is annoying; usage, artifacts, unarchive, environments, secrets, workers — add when a workflow needs them; stdio entrypoint — add if a client can't do HTTP
