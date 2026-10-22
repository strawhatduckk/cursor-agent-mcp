# cursor-agent-mcp

MCP server for [Cursor Cloud Agents](https://cursor.com/docs/cloud-agent/api/endpoints), served over Streamable HTTP from Docker.

## Run

```sh
docker build -t cursor-agent-mcp .
docker run --rm -p 127.0.0.1:8080:8080 -e CURSOR_API_KEY cursor-agent-mcp
```

Keep the `127.0.0.1:` prefix: with `CURSOR_API_KEY` set, anyone who can reach the port uses your key. Requests must use `localhost`, `127.0.0.1` or `[::1]` as the host (DNS rebinding protection), so `host.docker.internal` or compose service names get a 403.

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
