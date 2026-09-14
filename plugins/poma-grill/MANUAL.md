# Install guide

This plugin ships three things:

- **`skills/grill-ingest`** and **`skills/grill-search`** — instructions the agent loads when a task matches.
- **One MCP server** — the hosted POMA Grill endpoint at `https://mcp.poma-ai.com`, which provides the `grill_*` tools.

Skills are portable across clients. The MCP server has to be declared in each client's
own config format, so the same server appears three times below with different syntax.

## Quickest path: the `plugins` CLI

[`vercel-labs/plugins`](https://github.com/vercel-labs/plugins) installs an Agent Plugins
package into whichever agent tools it detects, translating the vendor-neutral format into
each one's native layout:

```bash
npx plugins add https://github.com/poma-ai/poma-grill-mcp
```

Point it at the repository, not at this directory — the CLI shallow-clones the repo to
`~/.cache/plugins/` and scans it, finding `poma-grill` under `plugins/`. Use the full
`https://github.com/...` URL; the `owner/repo` shorthand fails to resolve this repo.

It targets Claude Code, Cursor, Codex, Grok Build, Kimi Code, GitHub Copilot CLI, and VS
Code. Restrict it with `--target claude-code` and choose `--scope user|project|local`.
`npx plugins discover https://github.com/poma-ai/poma-grill-mcp` previews what it found
without installing; `npx plugins targets` lists the tools it detected on your machine.

For Claude Code it registers a one-plugin marketplace and installs from it, so the plugin
shows up in `claude plugin list` and `/plugin`. Remove it with
`claude plugin marketplace remove poma-grill`, which takes the installed plugin with it.

**One caveat, measured rather than assumed:** the CLI's Claude Code translation copies
`skills/` but does not generate a `.mcp.json`, so a plugin that declares its MCP server
only in the standard `mcp.json` installs with skills and no tools. This plugin ships its
own `.mcp.json`, so the server does come across — that file is the reason. Same for
`.claude-plugin/plugin.json`: without it the generated manifest carries an empty
description and version `0.0.0`.

The manual routes below need no extra tooling and are worth knowing when the CLI's
translation does not do what you want. They all reference a local clone:

```bash
git clone https://github.com/poma-ai/poma-grill-mcp && export POMA_PLUGIN="$PWD/poma-grill-mcp/plugins/poma-grill"
```

`$POMA_PLUGIN` stands in for that path in every command below. Set it again in each new
shell, or substitute the path directly.

## Which file each client reads

The plugin carries two manifests on purpose — the portable one and Claude Code's.

| File | Purpose | Claude Code | Cursor | Codex |
|---|---|---|---|---|
| `plugin.json` | [Agent Plugins 1.0.0](https://agent-plugins.org) manifest | ignored | ignored | ignored |
| `mcp.json` | Agent Plugins MCP declaration | ignored | ignored | ignored |
| `.claude-plugin/plugin.json` | Claude Code manifest | **read** | ignored | ignored |
| `.mcp.json` | Claude Code MCP declaration | **read** | ignored | ignored |
| `skills/<name>/SKILL.md` | Agent Skills | **read** | copy into `.cursor/skills/` | copy into `~/.codex/skills/` |

Claude Code does not implement the Agent Plugins standard: it reads `.mcp.json`, not
`mcp.json`, and it wants `"type": "http"` where the standard says `"streamable-http"`.
Declaring an MCP server only in `mcp.json` loads nothing in Claude Code, silently — and
the `plugins` CLI does not bridge that gap either. Keep the two MCP files in sync when you
change the endpoint.

---

## Claude Code

Three routes: `npx plugins add` (above), a one-session flag, or a copy into your skills
directory. All three end up with the same two skills and one MCP server.

### Try it for one session

```bash
claude --plugin-dir "$POMA_PLUGIN"
```

The skills become `/poma-grill:grill-ingest` and `/poma-grill:grill-search`; the server
registers as `plugin:poma-grill:poma-grill`. Run `/reload-plugins` after editing a file.

### Install it permanently

Copy the plugin into your skills directory. It auto-loads in every session, no
marketplace and no flag:

```bash
cp -R "$POMA_PLUGIN" ~/.claude/skills/poma-grill
```

Restart Claude Code, then authorize the server with `/mcp` — the first connection returns
a `401` and walks you through a browser login at
[console.poma-ai.com](https://console.poma-ai.com).

Symlink instead of copying if you want edits in the repo to take effect immediately:

```bash
ln -s "$POMA_PLUGIN" ~/.claude/skills/poma-grill
```

Remove it with `rm -rf ~/.claude/skills/poma-grill`.

### Verify

```bash
claude plugin validate "$POMA_PLUGIN"
```

Then, in a session, `/help` lists the two skills under the `poma-grill` namespace and
`/mcp` shows `poma-grill` as connected.

### Distribute to a team

The repo root carries a `.claude-plugin/marketplace.json` naming the marketplace
`poma-ai`, so teammates install straight from GitHub:

```bash
claude plugin marketplace add poma-ai/poma-grill-mcp && claude plugin install poma-grill@poma-ai
```

Add `--scope project` to the marketplace command to declare it for a repo rather than for
your user. Undo with `claude plugin marketplace remove poma-ai`, which takes the installed
plugin with it.

`npx plugins add https://github.com/poma-ai/poma-grill-mcp` registers its own one-off
marketplace instead, and needs no prior setup.

### API key instead of OAuth

Add headers to `.mcp.json`:

```json
{
  "mcpServers": {
    "poma-grill": {
      "type": "http",
      "url": "https://mcp.poma-ai.com",
      "headers": { "x-api-key": "your-api-key" }
    }
  }
}
```

---

## Cursor

Cursor reads skills from `.cursor/skills/` (project) or `~/.cursor/skills/` (global), and
MCP servers from `.cursor/mcp.json` or `~/.cursor/mcp.json`. It does not read a plugin
directory, so point it at the two pieces separately.

### Skills

```bash
mkdir -p ~/.cursor/skills && ln -s "$POMA_PLUGIN/skills/grill-ingest" ~/.cursor/skills/grill-ingest && ln -s "$POMA_PLUGIN/skills/grill-search" ~/.cursor/skills/grill-search
```

Use `.cursor/skills/` inside a repo instead when the skills should only apply to that
project. Cursor also accepts `.agents/skills/` and `~/.agents/skills/`.

### MCP server

Add to `~/.cursor/mcp.json`:

```json
{
  "mcpServers": {
    "poma-grill": {
      "url": "https://mcp.poma-ai.com"
    }
  }
}
```

Cursor infers the transport from `url`, so there is no `type` field. For an API key
instead of OAuth:

```json
{
  "mcpServers": {
    "poma-grill": {
      "url": "https://mcp.poma-ai.com",
      "headers": { "x-api-key": "your-api-key" }
    }
  }
}
```

Check Settings → MCP; the server should list the `grill_*` tools once authorized.

---

## Codex

Codex reads skills from `~/.codex/skills/` (personal) or `.agents/skills/` (project, also
searched at the repo root), and MCP servers from `~/.codex/config.toml`.

### Skills

```bash
mkdir -p ~/.codex/skills && ln -s "$POMA_PLUGIN/skills/grill-ingest" ~/.codex/skills/grill-ingest && ln -s "$POMA_PLUGIN/skills/grill-search" ~/.codex/skills/grill-search
```

Invoke one explicitly with `$grill-ingest`, or let Codex match it against the skill's
`description`.

### MCP server

Add to `~/.codex/config.toml`:

```toml
[mcp_servers.poma-grill]
url = "https://mcp.poma-ai.com"
bearer_token_env_var = "POMA_API_KEY"
```

Then `export POMA_API_KEY=your-api-key`. Dropping `bearer_token_env_var` makes Codex use
its default OAuth mode instead.

Verify with `codex mcp list`.

---

## Running the server locally instead

The hosted endpoint runs on POMA's infrastructure and cannot read paths on your machine,
so `file_path` ingestion does not work against it — use `url` or `file_base64` there, or
run the binary yourself and point the client at stdio. Claude Code, in `.mcp.json`:

```json
{
  "mcpServers": {
    "poma-grill": {
      "command": "poma-grill-mcp",
      "args": ["-input", "-"],
      "env": { "POMA_API_KEY": "your-api-key" }
    }
  }
}
```

Cursor takes the same shape in `mcp.json`. For Codex:

```toml
[mcp_servers.poma-grill]
command = "poma-grill-mcp"
args = ["-input", "-"]

[mcp_servers.poma-grill.env]
POMA_API_KEY = "your-api-key"
```

Install the binary per the [repo README](https://github.com/poma-ai/poma-grill-mcp#readme).

---

## Troubleshooting

| Symptom | Cause |
|---|---|
| Skills load, no `grill_*` tools | The MCP server is registered but unauthorized. Authorize it (`/mcp` in Claude Code) or set an API key header. |
| Nothing loads in Claude Code | Wrong directory passed to `--plugin-dir`. It must be `plugins/poma-grill`, the directory holding `skills/`. |
| Installed via `npx plugins add`, skills work, no tools | The plugin's `.mcp.json` is missing. The CLI does not generate one for Claude Code. |
| Server silently absent in Claude Code | `.mcp.json` is missing, or its server uses `"type": "streamable-http"`. Claude Code needs `"type": "http"`. |
| `401` on every call | The API key is rejected or expired. Generate a new one at [console.poma-ai.com](https://console.poma-ai.com). |
| Search returns nothing for a document you ingested | Ingest and search resolved to different projects. Check with `grill_projects`, then pass `project_id`. |
