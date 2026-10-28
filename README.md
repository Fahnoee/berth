# ⚓ berth

**Stable ports and process hygiene for coding agents and git worktrees.**

![berth demo: per-worktree ports, berth run, berth ls, and reaping abandoned servers in the dashboard](demo/demo.gif)

Run three agents in three worktrees and they all reach for `:3000`. Two of them
crash with `EADDRINUSE`; the third "fixes" it by killing the other's server.
An hour later you have six `vite` processes, a `uvicorn` from a worktree you
deleted yesterday and no idea which agent started what.

berth is a single static binary that acts as the harbor master:

- **Every worktree gets its own stable ports.** `berth port web` returns the
  same port for this worktree every time (bookmarks and `.env` files keep
  working) and never one another worktree holds.
- **Every dev server is tracked.** `berth run` injects `$PORT`, records which
  worktree, branch and agent session started the process, and can detach it
  with its output going to a log file.
- **Leftovers get found and reaped.** berth notices when the agent that
  started a server has exited, its worktree was deleted, it outlived its
  `--ttl`, or it's an orphan someone backgrounded with `&`.
- **Agents can drive it themselves** through the built-in MCP server.
- **You can watch it all** in a live terminal dashboard.

```
 ⚓ berth  4 processes · 5 ports · 1 abandoned
  SERVICE PID PORTS         AGENT   AGE STATUS             COMMAND
~/code/app  main  2
▌ web     101 :20341        claude   3m ok                 npm run dev -- --port 20341
  vite    303 :5173         -        1h detached           node vite
~/code/app-feat  feat/login  1
  api     202 :20342,:20343 codex ×  2h agent-gone expired go run ./cmd/api
outside any repo  1
  python3 900 :8000         -        4d ok                 python3 -m http.server

q quit  ↑↓ move  x stop  R reap  a abandoned  / filter  l logs  o open  enter details
```

No daemon, no config, no root. State is one JSON file guarded by a file lock,
so concurrent agents in different worktrees can't hand out the same port.
Linux and macOS.

## Install

```sh
go install github.com/Fahnoee/berth@latest
# or
nix run github:Fahnoee/berth
```

## Usage

```sh
berth run -- npm run dev                 # foreground, $PORT set, Ctrl-C works as usual
berth run -d --name api -- uvicorn app:app --port {port}   # detached, logs to a file
berth run -d --ttl 2h -- python -m http.server {port}      # reap it after 2 hours

PORT=$(berth port web) npm run dev       # just get this worktree's port
berth port web api --env >> .env.local   # WEB_PORT=…  API_PORT=…

berth                                    # live dashboard
berth ls                                 # everything listening, grouped by worktree
berth ls --here --json                   # just this worktree, machine-readable
berth logs api -f                        # follow a detached process's output
berth stop api                           # by service name (in this worktree), id, :port or pid
berth stop --here                        # stop everything in this worktree

berth reap -n                            # what looks abandoned?
berth reap --detached                    # also orphans that were backgrounded with &
berth reap --here && git worktree remove .   # clean up before removing a worktree
```

`berth run` exports `PORT` (rename with `--port-env`), `BERTH_PORT`,
`BERTH_SERVICE`, `BERTH_WORKTREE` and `BERTH_ID`, and replaces `{port}` in
arguments for tools that only take a flag. Running the same service twice in
a worktree is refused unless you pass `--replace`, which keeps agents from
stacking up copies of the same dev server.

### What counts as abandoned

| status          | meaning                                                                    |
| --------------- | -------------------------------------------------------------------------- |
| `agent-gone`    | the coding agent session that started it has exited                        |
| `worktree-gone` | its directory or worktree was deleted                                      |
| `expired`       | it outlived its `--ttl`                                                    |
| `detached`      | untracked, inside a repo, adopted by init or `systemd --user` (opt-in reap) |

Agents are recognised by walking up the process tree from the process (or from
`berth run`) looking for `claude`, `codex`, `cursor-agent`, `aider`,
`opencode`, `gemini`, `amp`, `goose` and friends. Add your own with
`BERTH_AGENTS=myagent,other`.

## For agents: MCP

```sh
claude mcp add berth -- berth mcp
```

or in any MCP client config:

```json
{ "mcpServers": { "berth": { "command": "berth", "args": ["mcp"] } } }
```

Tools: `berth_port`, `berth_run` (starts detached, waits until the port
accepts connections and returns the URL, or the log tail if it crashed),
`berth_ps`, `berth_logs`, `berth_stop` and `berth_reap`. The server's
instructions tell the agent to use `berth_run` instead of `&`/`nohup` and to
stop what it started.

Not using MCP? Put this in your `AGENTS.md` / `CLAUDE.md`:

```md
Start dev servers with `berth run -d -- <command>` (it prints the URL; use
`{port}` or $PORT instead of hardcoding ports). Check `berth ls --here`
before starting anything, and `berth stop --here` when you're done.
```

## Configuration

| variable                          | default                               |
| --------------------------------- | ------------------------------------- |
| `BERTH_HOME`                      | `$XDG_STATE_HOME/berth` or `~/.local/state/berth` |
| `BERTH_PORT_MIN`/`BERTH_PORT_MAX` | `20000`–`29999` (below the ephemeral range) |
| `BERTH_AGENTS`                    | extra agent executable names          |
| `BERTH_IGNORE`                    | untracked executables to hide, e.g. `postgres,docker-proxy` |

## License

MIT
