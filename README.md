# iknowledge

[中文](README.zh-CN.md) | **English**

**A code knowledge base for AI agents — the project's decision & experience archive.**

Give your AI a "project notebook". AI coding assistants have goldfish memory: once a project grows, they forget what lives where and how functions are meant to be called, re-read the code from scratch every session, forget it again, and — worst of all — forget *why* the last change was made, happily reverting fixes back into bugs. iknowledge captures the conclusions your AI paid real understanding-cost to reach, anchors them to code structure, and lets them decay and heal as the code evolves.

## What it records (value increases as you go down)

1. **The map** — what lives where (a project → directory → file → symbol pyramid, generated mechanically);
2. **The experience** — things the code itself doesn't tell you ("don't call this directly", "pass the password in plaintext, hashing happens inside");
3. **The ledger** — the *why* of every change and the alternatives that were **rejected** at the time. This is the anti-flip-flop layer: git doesn't record it, and nobody else in the world will record it for you.

Two iron laws: **knowledge navigates, source code decides** (the knowledge base never replaces reading the code); **the tool never touches your code**. Repository content is written only under `.knowledge/`; the only out-of-repo writes are per-repository user-private runtime state (auth/local identity/scout trust/crash WAL), an explicitly requested export artifact, and install/uninstall deployment. Changing source code is always the main AI's job.

## Easiest path: one command, then one sentence to your AI

```bash
curl -fsSL https://raw.githubusercontent.com/zdypro888/iknowledge/main/install.sh | sh
```

The installer does three things: installs a **checksummed prebuilt binary** when one is available (no Go toolchain needed; a missing checksum, missing verifier, or mismatch is never accepted), falls back to `go install` when no verified asset can be used, and installs the [`kb-bootstrap`](skills/kb-bootstrap/SKILL.md) skill into Claude Code (`~/.claude/skills/`) plus Codex (`~/.codex/skills/`) when detected. The release workflow builds macOS, Linux, and Windows for both amd64 and arm64. Set `IKNOWLEDGE_BIN` for a custom binary directory or `IKNOWLEDGE_FORCE_SOURCE=1` to build from source explicitly.

Then, inside any project, tell **Claude Code or Codex**: **"initialize the knowledge base for this project"**. The AI builds the skeleton, writes all integration config for you (the Claude Code trio + Codex's `config.toml`/`AGENTS.md`) and verifies connectivity (both clients field-tested). After restarting the session, the `kb_*` tools and hook injection are live; the server is auto-started on demand by the stdio bridge, so even a machine reboot needs no attention.

> The AI writing config for you does not violate the iron law: the *iknowledge binary* never edits source or integration files; its narrowly scoped private runtime state is kept outside the repository so secrets and crash recovery data cannot be committed accidentally. Prefer not to use the skill? Take the manual route below.

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
| `.mcp.json` | MCP stdio bridge (`command: iknowledge stdio`) | The agent sees the 16 `kb_*` tools; the bridge auto-starts the background serve on demand — zero service management (required) |
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
iknowledge doctor --repo . --deploy   # init/config/parser/deploy self-check; also flags accidental serve processes
iknowledge maintain --repo . --plan   # read-only debt roadmap (repayment goes through kb_maintain, done by the AI)
iknowledge import --repo . -i backup.kbundle --dry-run --backup   # preview/backup before bundle migration
# Existing different non-journal files require an explicit, reviewed --force; hard caps remain enforced.
git add .knowledge && git commit   # knowledge ships with the code: team-shared, branch-aware
iknowledge init --repo . --reanchor-all   # bulk re-anchor after a global change (e.g. repo-wide gofmt)
```

A screen full of `undigested` right after init is **by design**: skeleton first, knowledge gaps honestly labeled. When the AI hits one it says "skeleton only — read the source", automatically attaches the file's recent commit trail ("how it got here"), and never fabricates. To warm up the hot zones, `kb_status` ranks an undigested-hotspot list by *recent git churn × cross-file fan-in*; have the AI do one seeding pass over it (read hotspot files + `kb_remember`).

You never manage the server: the stdio bridge auto-starts the background serve on demand. Internal clients always verify the current loopback listener with a mutual-HMAC challenge and send only a scope-bound short-lived session; the long-term local identity is never sent to an unknown port. If Bearer auth was enabled before reboot, its user-private token also preserves that mode and the bridge restarts `serve --auth`. And even if everything is down, the AI just works normally and the hook stays silently inert.

## The 16 tools at a glance

| Kind | Tool | One-liner |
|---|---|---|
| Query | `kb_map` | Pyramid navigation: what lives where, coverage |
| Query | `kb_recall` | Knowledge / history / call relations and interface↔implementation (method-set matching) by keyword or node; hits auto-expand one hop along the call graph, flows & implementations; skeleton/suspect nodes get the commit trail attached |
| Query | `kb_diagnose` | Symptom/error → likely code locations, pitfalls, troubleshooting flows, and rejected-history context |
| Write | `kb_remember` | Distill experience (usage/pitfall/contract/summary…); supports declaring contradictions (disputes) for adjudication |
| Write | `kb_record_change` | The change ledger: what / why / what was rejected (one logical change = one record) |
| Write | `kb_verify` | confirm (upgrade confidence — evidence required, logged) / refute (with evidence; cascades to derived knowledge) / obsolete (graceful retirement) |
| Write | `kb_revert` | Transactional, append-only undo for an entirely wrong record_change / verify record; structured before/after effects make retries and crash recovery safe |
| Write | `kb_adopt` | Claim orphaned knowledge (symbol moved) or bury it (archive) |
| State | `kb_task` | Session-isolated work-in-progress ledger: start/update/complete, with end-of-task repayment & distillation reminders |
| State | `kb_flow` | Cross-file flow/topic nodes (login flow, payment chain…) |
| State | `kb_session` | Current-session summary and end-of-task gate for missing distillation / accounting risks |
| Scout | `kb_investigate` | Dispatch a disposable scout to locate things repo-wide; only conclusions come back — the main context stays clean |
| Scout | `kb_submit_findings` | The scout's report-back exit |
| Maint | `kb_status` | Library health: coverage / suspects / orphans / debts / hotspot list |
| Maint | `kb_maintain` | Claim maintenance debts (stale summaries, likely duplicates, pending re-verification, open disputes…); `patrol` returns a cross-node conflict-patrol brief |
| Maint | `kb_init` | In-library init/reconcile (equivalent to CLI init) |

## Uninstall (as painless as install)

```bash
# Per project: one sentence to the AI inside the project
# (stops the server, deletes .knowledge/, removes every integration trace; asks for confirmation first)
"uninstall this project's knowledge base"

# Machine level: removes the binary (including IKNOWLEDGE_BIN) and both skills, stops all running serves
curl -fsSL https://raw.githubusercontent.com/zdypro888/iknowledge/main/uninstall.sh | sh
```

Do the per-project sentence first, then the machine-level script (reversed order also works — the script ends by printing a manual-cleanup checklist). Machine uninstall removes local credentials/trust but preserves any prepared/committed crash WAL so the repository can still be recovered. If `.knowledge/` has been committed to git, think before deleting — that's a team-shared knowledge asset.

## FAQ

- **Won't this turn into the AI's junk-drawer memory?** No — **the knowledge base corresponds to code; it is not a memory store**. The one-question test: "would this become invalid if the code changed (or does it explain why this repo's code looks the way it does)?" Three things never belong: generic programming knowledge (true in any repo), session/user preferences (that's the AI host's own memory), and task to-dos (that's `kb_task`, git-excluded and disposable). Three layers enforce it: the discipline prompt, the tool descriptions, and write-time warnings (task-state words like TODO trigger a warning; every write to an anchor-less node — project/directory — surfaces the boundary reminder).
- **Is it Go-only?** Go has symbol-level parsing plus the full-repository call graph/interface matching. **Python** has AST-based symbols and semantic hashes (no call graph), isolated with `-I -S` and strict PEP 263 decoding. **JavaScript/TypeScript** (.ts/.tsx/.js/.jsx/.mjs/.cjs/.mts/.cts), **Rust**, and **Java** have built-in lightweight symbol lexers. **Any other language** can opt into file granularity with `extensions`; ledger, experience, injection and rot detection still work, but not symbol drill-down/call relations.
- **Should I "analyze the whole repo" first?** No. init builds only the structural skeleton (AST, free); semantic knowledge grows on demand. Bulk digestion is expensive, shallow, and starts rotting immediately — see "Cold start: a tower with holes" in the design docs.
- **What if the knowledge is wrong?** When the AI reads the source and finds a conflict, discipline says the source wins and it files a `kb_verify refute`; entries derived from the refuted one are cascade-downgraded to suspect. When two entries contradict each other and can't be settled on the spot, a *dispute* can be registered — both sides stay visible, each flagged "don't trust until adjudicated". Upgrading to *verified* is symmetric: `confirm` requires evidence too and leaves a journal record, so unverified claims can't be laundered into trusted ones. For contradictions living on *different* nodes, `kb_maintain patrol` clusters same-keyword knowledge across nodes into one brief for side-by-side adjudication.
- **Does knowledge go stale when code changes?** Yes — and the system knows. The anchor hash detects rot; a name-insensitive structural hash finds rename/move candidates; and a doc-sensitive migration guard prevents a rename combined with a contract change from being silently declared fresh. Ambiguous or unprovable migrations preserve the knowledge but mark it `suspect` pending re-verification. Re-reading a changed node within a session raises a staleness alert, and suspects enter the maintenance-debt queue.
- **Security model?** It listens on `127.0.0.1` by default; Origin validation blocks browser DNS-rebinding. Internal stdio/hook/scout traffic performs a loopback-only mutual-HMAC listener check even when business Bearer auth is off. On shared machines use `serve --auth`, which additionally requires Bearer or a scoped short session on business endpoints. Long-term keys and scout trust live outside the repository under the user's private config state, partitioned by the canonical repository path (Unix files 0600); legacy in-repo tokens are rotated, never reused. `.knowledge` writes and source reads reject symlinks beneath their respective roots, so a hostile checkout cannot redirect storage or make tracked symlinks disclose outside files. Plain HTTP on an explicitly non-loopback bind still does not provide transport confidentiality.
- **My agent host has no subagent capability — can I still use scouting?** `kb_investigate` defaults to delegate mode. If the host has none, set `scout: self`, review the command, then run `iknowledge trust-scout --repo .`. The authorization is user-private state outside the repository, bound to the exact mode/command and invalidated by any config change; repository-controlled executables are refused. The temporary in-repo MCP config contains only a short HMAC-derived session, never a root secret. macOS/Linux only.
- **My custom subagents (audit agents etc.) have no `kb_*` tools — how do they query?** Use the read-only legs: `curl "http://127.0.0.1:<port>/recall?q=<term>"` (also `/map`, `/status`) — anything with a shell can query, zero MCP config, output identical to the tools; investigation briefings include this fallback automatically. Read-only: recording and distillation still go through the main AI.
- **Does Codex work?** Yes, field-tested (codex-cli 0.142, including the desktop app): paste section ④ of `iknowledge setup` into `~/.codex/config.toml` (stdio form, `command = "iknowledge"`; the http-direct alternative — with `http_headers` under `serve --auth` — is printed too), and the discipline section into the repo's `AGENTS.md`. Two differences: Codex prompts once for MCP tool-call approval (click allow in the UI; headless `exec` needs `--dangerously-bypass-approvals-and-sandbox`), and it has no hook injection — it relies on discipline-driven querying.

## Status

Phase 1 fully delivered and continuously hardened: now 16 MCP tools + the `/mcp/main` and `/mcp/scout` endpoints + `GET /inject` and the read-only legs (`/recall` `/map` `/status`) + the `iknowledge hook/setup/maintain/doctor` suite. A 2026-07-11 adversarial audit additionally hardened crash-recoverable multi-file transactions, strict/portable bundles, parser boundaries and semantic hashes, generation-aware indexes, concurrent snapshots, source/storage symlink confinement, listener identity, self-scout trust, and checksummed cross-platform installation. On 2026-07-04 the originally-deferred phase 2/3/4 items landed: full-repo call graph & structural search expansion, hotspot digestion list, dispute registration, review reminders for non-code knowledge, `--auth`, multi-repo single daemon, Windows support, and the PTY self-dispatch scout fallback. **Both clients field-tested** (Claude Code + Codex, including instructions semantics). **M1.4 A/B acceptance passed**: 10 fixed code-location tasks, knowledge-base-connected (19 % seeded coverage) vs bare grep, same model — median tokens down 41 % (59 % ≤ the 60 % threshold), cheaper on 8/10 tasks, faster wall-clock; protocol, harness (`cmd/kbeval`) and both rounds of raw data live in [eval/m14/](eval/m14/).

- [`knowledge.md`](knowledge.md) — the concept design (the convergence of 20 design rounds: five dimensions, self-healing, economics, security, four thought-experiments) *(Chinese)*
- [`knowledge-impl.md`](knowledge-impl.md) — the phase-1 engineering spec (package layout, data model, storage, full MCP API spec, milestones) *(Chinese)*

## License

[MIT](LICENSE) — free for commercial and non-commercial use, modification, and redistribution. The only dependency is `gopkg.in/yaml.v3` (also MIT/APACHE-2.0).
