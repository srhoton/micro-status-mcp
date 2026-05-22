# micro-status-mcp

A small Go MCP server that lets Claude Code sessions running in different repos message each other and wake each other up via `tmux send-keys`.

Replaces the filesystem-backed `~/.claude/agent-mailbox/` mailbox (and the
`agent-msg` Bash script in `~/git/dotfiles`) with a single long-lived process
backed by SQLite.

## How it fits together

```
   Claude session (repo A)              Claude session (repo B)
   ┌───────────────────┐                 ┌───────────────────┐
   │ /post-msg → MCP   │                 │ /inbox    → MCP   │
   └────────┬──────────┘                 └────────▲──────────┘
            │ HTTP (streamable)                   │
            ▼                                     │
   ┌──────────────────────────── micro-status-mcp serve ──────────────┐
   │  MCP tools: register / unregister / post_message /               │
   │             list_messages / mark_read / list_sessions            │
   │                                                                  │
   │  SQLite (~/.claude/micro-status-mcp/state.db)                    │
   │  tmux send-keys notifier  ──► /inbox keystroke into target pane  │
   └──────────────────────────────────────────────────────────────────┘
```

## Install

```bash
go install github.com/stephenrhoton/micro-status-mcp/cmd/micro-status-mcp@latest
```

Or build locally:

```bash
make build           # produces ./bin/micro-status-mcp
make install         # installs into $GOBIN
```

## Run

The server is designed to be run in a dedicated tmux pane / terminal:

```bash
tmux new-window -n mcp 'micro-status-mcp serve'
```

By default it listens on `127.0.0.1:7878` and persists state at
`~/.claude/micro-status-mcp/state.db`. Both are configurable via `--addr` and
`--db`.

A health check is exposed at `http://127.0.0.1:7878/healthz` for scripting.

## Wire into Claude Code

Add an entry to your user-level `~/.claude.json` so every repo can reach the
server:

```jsonc
{
  "mcpServers": {
    "micro-status": {
      "type": "http",
      "url": "http://127.0.0.1:7878/mcp"
    }
  }
}
```

The dotfiles repo updates `~/.claude/scripts/session-env-setup.sh` to register
each Claude session's tmux pane on startup, and the `/post-msg` and `/inbox`
slash commands to call the MCP tools.

## Tools exposed

| Tool | Purpose |
|---|---|
| `register` | Announce `{repo, pane}` so the server can wake you. |
| `unregister` | Drop a registration. Idempotent. |
| `post_message` | Send `{from, to, subject, body}`. Wakes the recipient pane if alive. |
| `list_messages` | Read messages addressed to a repo. Defaults to unread only. |
| `mark_read` | Mark a single message read by id. |
| `list_sessions` | Show every registered `{repo, pane}` pair. |

## CLI

The same binary doubles as a thin MCP client for shell use:

```bash
micro-status-mcp register --repo "$(basename "$PWD")" --pane "$(tmux display-message -p '#S:#I.#P')"
micro-status-mcp post --from alpha --to beta --subject "hi" --body "world"
micro-status-mcp post --from alpha --to beta --subject "diff" --body-file -   # read body from stdin
micro-status-mcp list --repo beta --mark-read
micro-status-mcp sessions
```

All client subcommands accept `--endpoint` (defaults to
`http://127.0.0.1:7878/mcp`).

## Development

```bash
make test            # go test ./...
make vet
make lint            # requires golangci-lint
make fmt             # gofmt -s -w .
```
