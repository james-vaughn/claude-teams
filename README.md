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

The server keeps its state in a SQLite database, `team-server.db` by default. Sessions, members, messages and the server token all survive a restart, and message ids continue where they left off.

On first start the server generates a token, stores it in the database and prints it:

```
Server token: 3f9c...
```

Later starts reuse the stored token. Every request except `/healthz` must send it.

Flags:

| Flag | Default | Meaning |
|---|---|---|
| `-port` | `8080` | Port to listen on |
| `-db` | `team-server.db` | Path to the SQLite database file. It's created if it doesn't exist. |
| `-max-sessions` | `100` | Maximum number of sessions at the same time |
| `-rotate-token` | `false` | Generate a new token and save it in place of the stored one. Clients using the old token get `401`. |

To start over with no sessions and a new token, stop the server and delete the database files (`team-server.db`, `team-server.db-wal`, `team-server.db-shm`).

Expose it:

```bash
ngrok http 8080
```

The SQLite driver is pure Go (`modernc.org/sqlite`), so no cgo or system SQLite is needed. The database is meant for one server process. Don't point two servers at the same file.

Stopping a session removes all of its data from the database: the session, its members and its messages. The server uses SQLite `secure_delete`, which overwrites deleted content with zeros, and truncates the WAL after each stop, so the data can't be recovered from `team-server.db` or `team-server.db-wal`. The server's own console log still has a line for each create, join, publish and stop. Those lines include the session id, session name and member names, but never message text.

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
| `team_start_session` | `url`, `token`, `name`, `sessionName?` | Creates a session, joins it, and returns its `sessionId` to share. |
| `team_join` | `url`, `token`, `sessionId`, `name` | Joins an existing session and returns its members and `latestId`. Resets the read cursor to 0. |
| `team_stop_session` | none | Stops the session you are in. Deletes its log and removes every member. |
| `team_publish` | `text`, `kind?` (default `update`) | `{ id }` |
| `team_receive` | `since?`, `limit?` (default 100, max 500) | Messages with `id > since`, plus `cursor`, `latestId`, `hasMore`. Without `since` it continues from the last message received. |

An agent is in one session at a time. Starting or joining another session replaces the current one.

## Members

When an agent starts or joins a session, the server gives it a member token. `team-mcp` keeps the token in memory and sends it on every later request. The server uses it to decide who the agent is:

- The sender of a published message is the member that owns the token. Clients can't choose the sender.
- Only members can read messages, see session info or stop the session. The agent that starts a session joins it at the same time, so it's a member too.
- A name belongs to the first member that joins with it. Joining with a name that's already taken fails with `409`, unless the request sends that member's current token. A rejoin like that issues a new token and the old one stops working.

The server stores only a SHA-256 hash of each member token. Tokens aren't saved on disk by `team-mcp`, so after you restart the agent, join again with a new name.

## Usage

Agents only use these tools when you explicitly ask. They don't publish progress or read messages on their own.

The person who starts a session:

> Start a team session on `<url>` with token `<token>` as `claude-A`, named `inc-1234`.

The agent replies with a session id. Share the server URL, the token and the session id with your teammates. They join with:

> Join session `<session id>` on `<ngrok url>` with token `<token>` as `codex-B`.

Then:

> Share the current status with the team.

> Read the team messages.

> Stop the session.

## HTTP API

Every endpoint except `/healthz` requires two headers:

- `Authorization: Bearer <token>`
- `X-Team-MCP-Version: <version>`: the version of `team-mcp` making the request, like `0.2.0`. `team-mcp` sends it on every request. The server stores it on the member when they join, and session info shows it as `mcpVersion` on each member.

Endpoints marked "member" also require `X-Team-Member-Token: <memberToken>`, the token from the create or join response.

| Method | Path | Auth | Body / query | Response |
|---|---|---|---|---|
| `GET` | `/healthz` | none | none | `{"status": "ok"}` |
| `POST` | `/sessions` | server | `{"name": "...", "createdBy": "..."}` | `201` join response for `createdBy` |
| `GET` | `/sessions/{id}` | member | none | session info: `id`, `name`, `createdBy`, `createdAt`, `members` (`name`, `mcpVersion`, `joinedAt`, `lastSeen`), `latestId` |
| `DELETE` | `/sessions/{id}` | member | none | session info of the stopped session |
| `POST` | `/sessions/{id}/members` | server, plus member to rejoin | `{"name": "..."}` | join response: `{"session": {...}, "joinMessageId": n, "memberToken": "..."}` |
| `POST` | `/sessions/{id}/messages` | member | `{"kind": "...", "text": "..."}` | `201 {"id": n}` |
| `GET` | `/sessions/{id}/messages` | member | `?since=n&limit=m` | `{"messages": [...], "latestId": n, "hasMore": bool}` |

Errors:

- `400`: the request is invalid, or `X-Team-MCP-Version` is missing or isn't a version like `0.1.0`.
- `401`: the token is missing or wrong.
- `404`: the session doesn't exist or was stopped.
- `403`: the member token is missing or doesn't belong to a member of this session.
- `409`: another member already uses that name in the session.
- `503`: the server reached `-max-sessions`.

Names are 1-64 characters. Message text can be up to 16,000 characters.

## Test

```bash
go test -race ./...
```
