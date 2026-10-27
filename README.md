# claude-teams

Lets two or more coding agents (Claude Code, Codex, ...) on different machines share updates through a small relay server.

- `team-server` is an HTTP relay. It keeps an in-memory, per-team message log and gives every message an incrementing id.
- `team-mcp` is a stdio MCP server each agent runs locally. It exposes `team_join`, `team_publish` and `team_receive`.

Delivery is pull-based: an agent sees new messages when it calls `team_receive`.

## Build

```bash
go build -o bin/team-server ./cmd/team-server
go build -o bin/team-mcp ./cmd/team-mcp
```

## Run the server

```bash
./bin/team-server -port 8080 -token "$(openssl rand -hex 16)"
```

`-token` is optional (it also reads `$TEAM_TOKEN`). With ngrok the server is on the public internet, so use a token.

Expose it:

```bash
ngrok http 8080
```

Share the `https://....ngrok-free.app` URL and the token with your teammates.

Messages live in memory only. Restarting the server clears every team.

## Add the MCP to an agent

Claude Code:

```bash
claude mcp add claude-teams -- /Users/you/Git/claude-teams/bin/team-mcp
```

Codex (`~/.codex/config.toml`):

```toml
[mcp_servers.claude-teams]
command = "/Users/you/Git/claude-teams/bin/team-mcp"
```

## Tools

| Tool | Arguments | Result |
|---|---|---|
| `team_join` | `url`, `team`, `name`, `token?` | Posts a `join` message and returns `latestId`. Resets the read cursor to 0. |
| `team_publish` | `text`, `kind?` (default `update`) | `{ id }` |
| `team_receive` | `since?`, `limit?` (default 100, max 500) | Messages with `id > since`, plus `cursor`, `latestId`, `hasMore`. Without `since` it continues from the last message received. |

## Usage

Agents only use these tools when you explicitly ask. They don't publish progress or read messages on their own. Example prompts:

> Join team `inc-1234` at `<ngrok url>` as `claude-A` with token `<token>`.

> Share the current status with the team.

> Read the team messages.

## HTTP API

| Method | Path | Body / query |
|---|---|---|
| `GET` | `/healthz` | none |
| `POST` | `/teams/{team}/messages` | `{"from": "...", "kind": "...", "text": "..."}` returns `201 {"id": n}` |
| `GET` | `/teams/{team}/messages` | `?since=n&limit=m` returns `{"messages": [...], "latestId": n, "hasMore": bool}` |

Team names are 1-64 characters from `A-Z a-z 0-9 - _`. Message text can be up to 16,000 characters. When a token is set, send it as `Authorization: Bearer <token>`.

## Test

```bash
go test -race ./...
```
