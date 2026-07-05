# iknowledge

[中文](README.zh-CN.md) | **English**

**A code knowledge base for AI agents — the project's decision & experience archive.**

Give your AI a "project notebook". AI coding assistants have goldfish memory: once a project grows, they forget what lives where and how functions are meant to be called, re-read the code from scratch every session, forget it again, and — worst of all — forget *why* the last change was made, happily reverting fixes back into bugs. iknowledge captures the conclusions your AI paid real understanding-cost to reach, anchors them to code structure, and lets them decay and heal as the code evolves.

## What it records (value increases as you go down)

1. **The map** — what lives where (a project → directory → file → symbol pyramid, generated mechanically);
2. **The experience** — things the code itself doesn't tell you ("don't call this directly", "pass the password in plaintext, hashing happens inside");
3. **The ledger** — the *why* of every change and the alternatives that were **rejected** at the time. This is the anti-flip-flop layer: git doesn't record it, and nobody else in the world will record it for you.

Two iron laws: **knowledge navigates, source code decides** (the knowledge base never replaces reading the code); **the tool never touches your code** (read-only on sources; the only thing it ever writes is `.knowledge/` — changing code is always the main AI's job).

## Easiest path: one command, then one sentence to your AI

```bash
curl -fsSL https://raw.githubusercontent.com/zdypro888/iknowledge/main/install.sh | sh
```

The installer does three things: `go install`s the binary, installs the [`kb-bootstrap`](skills/kb-bootstrap/SKILL.md) skill into Claude Code (`~/.claude/skills/`), and — if Codex is detected — into `~/.codex/skills/` as well.

Then, inside any project, tell **Claude Code or Codex**: **"initialize the knowledge base for this project"**. The AI builds the skeleton, writes all integration config for you (the Claude Code trio + Codex's `config.toml`/`AGENTS.md`) and verifies connectivity (both clients field-tested). After restarting the session, the `kb_*` tools and hook injection are live; the server is auto-started on demand by the stdio bridge, so even a machine reboot needs no attention.

> The AI writing config for you does not violate the iron law: the law constrains the *iknowledge binary* (which only ever writes `.knowledge/`); pasting config was designed to be done "by the user or the main AI". Prefer not to use the skill? Take the manual route below.

## Manual setup in 30 seconds

```bash
# 1. Install (requires Go; or git clone && go build ./cmd/iknowledge)
go install github.com/zdypro888/iknowledge/cmd/iknowledge@latest
iknowledge version    # verify

# 2. Initialize your repo (pure-AST skeleton, zero LLM cost; ~13 s measured on a 480k-line repo)
iknowledge init --repo /path/to/your/repo

# 3. Print the three integration snippets; paste each where indicated (iknowledge only prints — it never writes your files)
iknowledge setup --repo /path/to/your/repo

# (No manual server start needed: with the stdio form in .mcp.json,
#  the first AI session brings the background serve up automatically)
```

The three snippets printed by `setup` go into three files of the target repo:

| Where | What | Why |
|---|---|---|
| `.mcp.json` | MCP stdio bridge (`command: iknowledge stdio`) | The agent sees the 13 `kb_*` tools; the bridge auto-starts the background serve on demand — zero service management (required) |
| `CLAUDE.md` | Discipline prompt | The working rules for the AI: query before locating, record after changing, distill what was hard-won (required) |
| `.claude/settings.json` | Hook snippet | Every time the AI Reads/Edits a file, that file's knowledge + staleness alerts are injected into context automatically (recommended) |

Multiple repos coexist naturally: each repo gets its own port (`18000 + hash(path) % 2000`), and one process can serve several repos at once (`iknowledge serve --repo A --repo B` — each repo keeps its own port, so no client config changes).

<details>
<summary><b>Manual daemon / start on boot (optional)</b> — the stdio bridge already manages the server; only needed for remote or explicitly-shared setups</summary>

macOS (launchd): save as `~/Library/LaunchAgents/com.iknowledge.serve.plist`, then `launchctl load` it:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.iknowledge.serve</string>
  <key>ProgramArguments</key><array>
    <string>/Users/you/go/bin/iknowledge</string>
    <string>serve</string>
    <string>--repo</string><string>/path/to/repoA</string>
    <string>--repo</string><string>/path/to/repoB</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
```

Linux (systemd user unit): save as `~/.config/systemd/user/iknowledge.service`, then `systemctl --user enable --now iknowledge`:

```ini
[Unit]
Description=iknowledge knowledge MCP

[Service]
ExecStart=%h/go/bin/iknowledge serve --repo /path/to/repoA --repo /path/to/repoB
Restart=on-failure

[Install]
WantedBy=default.target
```

</details>

## Day-to-day usage

**Mostly hands-off after install.** While the AI works in your repo: the hook feeds it the knowledge of every file it touches (and reminds it to record right after an edit); the discipline prompt makes it `kb_recall` before locating, `kb_record_change` after changing, and `kb_remember` whatever took real effort to understand — **the knowledge base grows from real work** (nodes are created on first touch; digestion cost piggybacks on tasks you were doing anyway).

You only occasionally need:

```bash
iknowledge status --repo .     # coverage / freshness / maintenance debts + hotspot digestion list (git churn × cross-file fan-in)
iknowledge maintain --repo .   # read-only debt listing (repayment goes through kb_maintain, done by the AI)
git add .knowledge && git commit   # knowledge ships with the code: team-shared, branch-aware
iknowledge init --repo . --reanchor-all   # bulk re-anchor after a global change (e.g. repo-wide gofmt)
```

A screen full of `undigested` right after init is **by design**: skeleton first, knowledge gaps honestly labeled. When the AI hits one it says "skeleton only — read the source", automatically attaches the file's recent commit trail ("how it got here"), and never fabricates. To warm up the hot zones, `kb_status` ranks an undigested-hotspot list by *recent git churn × cross-file fan-in*; have the AI do one seeding pass over it (read hotspot files + `kb_remember`).

You never manage the server: the stdio bridge auto-starts the background serve on demand (the first session after a reboot brings it back). And even if everything is down, the AI just works normally and the hook stays silently inert.

## The 13 tools at a glance

| Kind | Tool | One-liner |
|---|---|---|
| Query | `kb_map` | Pyramid navigation: what lives where, coverage |
| Query | `kb_recall` | Knowledge / history / call relations and interface↔implementation (method-set matching) by keyword or node; hits auto-expand one hop along the call graph, flows & implementations; skeleton/suspect nodes get the commit trail attached |
| Write | `kb_remember` | Distill experience (usage/pitfall/contract/summary…); supports declaring contradictions (disputes) for adjudication |
| Write | `kb_record_change` | The change ledger: what / why / what was rejected (one logical change = one record) |
| Write | `kb_verify` | confirm (upgrade confidence) / refute (with evidence; cascades to derived knowledge) / obsolete (graceful retirement) |
| Write | `kb_adopt` | Claim orphaned knowledge (symbol moved) or bury it (archive) |
| State | `kb_task` | Work-in-progress ledger: start/update/complete, with end-of-task repayment & distillation reminders |
| State | `kb_flow` | Cross-file flow/topic nodes (login flow, payment chain…) |
| Scout | `kb_investigate` | Dispatch a disposable scout to locate things repo-wide; only conclusions come back — the main context stays clean |
| Scout | `kb_submit_findings` | The scout's report-back exit |
| Maint | `kb_status` | Library health: coverage / suspects / orphans / debts / hotspot list |
| Maint | `kb_maintain` | Claim maintenance debts (stale summaries, likely duplicates, pending re-verification, open disputes…) |
| Maint | `kb_init` | In-library init/reconcile (equivalent to CLI init) |

## Uninstall (as painless as install)

```bash
# Per project: one sentence to the AI inside the project
# (stops the server, deletes .knowledge/, removes every integration trace; asks for confirmation first)
"uninstall this project's knowledge base"

# Machine level: removes the binary and both skills, stops all running serves
curl -fsSL https://raw.githubusercontent.com/zdypro888/iknowledge/main/uninstall.sh | sh
```

Do the per-project sentence first, then the machine-level script (reversed order also works — the script ends by printing a manual-cleanup checklist). If `.knowledge/` has been committed to git, think before deleting — that's a team-shared knowledge asset.

## FAQ

- **Won't this turn into the AI's junk-drawer memory?** No — **the knowledge base corresponds to code; it is not a memory store**. The one-question test: "would this become invalid if the code changed (or does it explain why this repo's code looks the way it does)?" Three things never belong: generic programming knowledge (true in any repo), session/user preferences (that's the AI host's own memory), and task to-dos (that's `kb_task`, git-excluded and disposable). Three layers enforce it: the discipline prompt, the tool descriptions, and write-time warnings (task-state words like TODO trigger a warning; every write to an anchor-less node — project/directory — surfaces the boundary reminder).
- **Is it Go-only?** Symbol-level support (function granularity, format-immune hashing, rename migration, call graph): Go is complete, and Python is supported too (parsed by your local python3 using its own ast module — zero extra installs; dual hashing and rename migration fully work). **Any other language**: add `extensions: [".proto", ".sql", ".ts"]` to `.knowledge/config.yaml` and those files join at file granularity — ledger, experience, hook injection and rot detection (content hash) all work; you just don't get symbol drill-down or the call graph. The core engine is language-agnostic; adding a symbol-level language = writing one parser plugin.
- **Should I "analyze the whole repo" first?** No. init builds only the structural skeleton (AST, free); semantic knowledge grows on demand. Bulk digestion is expensive, shallow, and starts rotting immediately — see "Cold start: a tower with holes" in the design docs.
- **What if the knowledge is wrong?** When the AI reads the source and finds a conflict, discipline says the source wins and it files a `kb_verify refute`; entries derived from the refuted one are cascade-downgraded to suspect. When two entries contradict each other and can't be settled on the spot, a *dispute* can be registered — both sides stay visible, each flagged "don't trust until adjudicated".
- **Does knowledge go stale when code changes?** Yes — and the system knows: dual-hash anchoring detects rot, renames/moves migrate automatically, mismatches are flagged `suspect` pending re-verification; re-reading a changed node within a session raises a staleness alert; suspects enter the maintenance-debt queue, so nothing rots unattended.
- **Security model?** Listens on `127.0.0.1` only by default, no auth (local trust model); Origin validation blocks browser DNS-rebinding; listening on a non-loopback address prints a warning. On shared multi-user machines use `serve --auth`: a token is generated at `.knowledge/local/token` (0600), all endpoints require `Authorization: Bearer`, and `setup` prints integration snippets with the header included (contains the secret — don't commit it). The tool remains read-only on your sources.
- **My agent host has no subagent capability — can I still use scouting?** `kb_investigate` defaults to delegate mode (the briefing is handed to a host subagent). If the host has none, set `scout: self` in `.knowledge/config.yaml` — the server spawns a scout process itself under a PTY (default `claude`; configurable via `scout_command`), runs the briefing, and blocks until the report is filed, so one call returns the conclusions. macOS/Linux only.
- **My custom subagents (audit agents etc.) have no `kb_*` tools — how do they query?** Use the read-only legs: `curl "http://127.0.0.1:<port>/recall?q=<term>"` (also `/map`, `/status`) — anything with a shell can query, zero MCP config, output identical to the tools; investigation briefings include this fallback automatically. Read-only: recording and distillation still go through the main AI.
- **Does Codex work?** Yes, field-tested (codex-cli 0.142, including the desktop app): paste section ④ of `iknowledge setup` into `~/.codex/config.toml` (stdio form, `command = "iknowledge"`; the http-direct alternative — with `http_headers` under `serve --auth` — is printed too), and the discipline section into the repo's `AGENTS.md`. Two differences: Codex prompts once for MCP tool-call approval (click allow in the UI; headless `exec` needs `--dangerously-bypass-approvals-and-sandbox`), and it has no hook injection — it relies on discipline-driven querying.

## Status

Phase 1 fully delivered and continuously hardened: 13 MCP tools + the `/mcp/main` and `/mcp/scout` endpoints + `GET /inject` and the read-only legs (`/recall` `/map` `/status`) + the `iknowledge hook/setup/maintain` suite, fixed through multiple adversarial reviews and a third-party full-repo audit. On 2026-07-04 the originally-deferred phase 2/3/4 items landed: full-repo call graph & structural search expansion, hotspot digestion list, dispute registration, review reminders for non-code knowledge, `--auth`, multi-repo single daemon, Windows support (CI green on all three OSes), and the PTY self-dispatch scout fallback. **Both clients field-tested** (Claude Code + Codex, including instructions semantics). **M1.4 A/B acceptance passed**: 10 fixed code-location tasks, knowledge-base-connected (19 % seeded coverage) vs bare grep, same model — median tokens down 41 % (59 % ≤ the 60 % threshold), cheaper on 8/10 tasks, faster wall-clock; protocol, harness (`cmd/kbeval`) and both rounds of raw data live in [eval/m14/](eval/m14/).

- [`knowledge.md`](knowledge.md) — the concept design (the convergence of 20 design rounds: five dimensions, self-healing, economics, security, four thought-experiments) *(Chinese)*
- [`knowledge-impl.md`](knowledge-impl.md) — the phase-1 engineering spec (package layout, data model, storage, full MCP API spec, milestones) *(Chinese)*
