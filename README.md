# claude-teams

Lets two or more coding agents (Claude Code, Codex, ...) on different machines share updates through a small relay server.

- `team-server` is an HTTP relay. It hosts any number of isolated team sessions. Each session has a UUID, its own members and its own message log, and gives every message an incrementing id.
- `team-mcp` is a stdio MCP server each agent runs locally. It exposes `team_start_session`, `team_join`, `team_stop_session`, `team_publish` and `team_receive`.

Delivery is pull-based: an agent sees new messages when it calls `team_receive`.

## Build

```bash
./build.sh
```

This runs `go build` for both executables and writes them to `bin/`. It works from any directory.

## Run the server

```bash
./bin/team-server -port 8080
```

Flags:

| Flag | Default | Meaning |
|---|---|---|
| `-port` | `8080` | Port to listen on |
| `-db` | `team-server.db` | Path to the SQLite database file. It's created if it doesn't exist. |

To start over with no sessions, stop the server and delete the database files (`team-server.db`, `team-server.db-wal`, `team-server.db-shm`).


## Add the MCP to an agent

`team-mcp` reads the team server's base URL from the `TEAM_SERVER_URL` environment variable. Use whatever URL the server is reachable at, such as a reverse proxy, a tunnel or a host on your network. Every member of a session must point at the same server. If the variable is missing or isn't an http(s) URL, the tools return an error that says so.

Claude Code:

```bash
claude mcp add claude-teams -e TEAM_SERVER_URL=https://your.url.com -- /Users/you/Git/claude-teams/bin/team-mcp
```

Codex (`~/.codex/config.toml`):

```toml
[mcp_servers.claude-teams]
command = "/Users/you/Git/claude-teams/bin/team-mcp"
env = { TEAM_SERVER_URL = "https://your.url.com" }
```

When the server URL changes, update the variable and restart the agent.

## Tools

| Tool | Arguments | Result |
|---|---|---|
| `team_start_session` | `name`, `sessionName?` | Creates a session, joins it, and returns its `sessionId` and `sessionToken` to share. |
| `team_join` | `sessionId`, `sessionToken`, `name` | Joins an existing session and returns its members and `latestId`. Resets the read cursor to 0. |
| `team_stop_session` | none | Stops the session you are in. Deletes its log and removes every member. |
| `team_publish` | `text`, `kind?` (default `update`) | `{ id }` |
| `team_receive` | `since?`, `limit?` (default 100, max 500) | Messages with `id > since`, plus `cursor`, `latestId`, `hasMore`. Without `since` it continues from the last message received. |

An agent is in one session at a time. Starting or joining another session replaces the current one.


## Usage

Agents only use these tools when you explicitly ask. They don't publish progress or read messages on their own.

The person who starts a session:

> Start a team session as `claude-A`, named `inc-1234`.

The agent replies with a session id and a session token. Share the server URL, the session id and the session token with your teammates. They set the URL as `TEAM_SERVER_URL` in their MCP config. Anyone with the session token can join, so share it only with them. They join with:

> Join session `<session id>` with session token `<session token>` as `codex-B`.

Then:

> Share the current status with the team.

> Read the team messages.

> Stop the session.

## HTTP API

Every endpoint except `/healthz` requires `X-Team-MCP-Version: <version>`: the version of `team-mcp` making the request, like `0.3.0`. `team-mcp` sends it on every request. The server stores it on the member when they join, and session info shows it as `mcpVersion` on each member.

Endpoints marked "session" require `X-Team-Session-Token: <sessionToken>`, the token from the create response. Endpoints marked "member" require `X-Team-Member-Token: <memberToken>`, the token from the create or join response.

| Method | Path | Auth | Body / query | Response |
|---|---|---|---|---|
| `GET` | `/healthz` | none | none | `{"status": "ok"}` |
| `POST` | `/sessions` | none | `{"name": "...", "createdBy": "..."}` | `201` join response for `createdBy`, plus `sessionToken` |
| `GET` | `/sessions/{id}` | member | none | session info: `id`, `name`, `createdBy`, `createdAt`, `members` (`name`, `mcpVersion`, `joinedAt`, `lastSeen`), `latestId` |
| `DELETE` | `/sessions/{id}` | member | none | session info of the stopped session |
| `POST` | `/sessions/{id}/members` | session, plus member to rejoin | `{"name": "..."}` | join response: `{"session": {...}, "joinMessageId": n, "memberToken": "..."}` |
| `POST` | `/sessions/{id}/messages` | member | `{"kind": "...", "text": "..."}` | `201 {"id": n}` |
| `GET` | `/sessions/{id}/messages` | member | `?since=n&limit=m` | `{"messages": [...], "latestId": n, "hasMore": bool}` |

Errors:

- `400`: the request is invalid, or `X-Team-MCP-Version` is missing or isn't a version like `0.1.0`.
- `401`: the session token is missing or wrong.
- `404`: the session doesn't exist or was stopped.
- `403`: the member token is missing or doesn't belong to a member of this session.
- `409`: another member already uses that name in the session.

Names are 1-64 characters. Message text can be up to 16,000 characters.

## Test

```bash
go test -race ./...
```
