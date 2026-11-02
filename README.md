<div align="center">

<img src="docs/assets/banner.png" alt="EVOLV — the workspace that rewrites itself. A self-evolving AI workspace: it grows its own UI, tools, skills and prompts live while you watch." width="100%">

# EVOLV — the self-evolving AI workspace that rewrites itself

**EVOLV is a self-evolving AI agent workspace: a self-modifying coding agent that grows its own UI, tools, skills, prompts and plugins live while you watch.** Local-first, single Go binary, works with Ollama or any API key — and the demo below needs none.

[![GitHub stars](https://img.shields.io/github/stars/dxn111/EVOLV?style=flat-square&color=8b5cf6)](https://github.com/dxn111/EVOLV/stargazers)
[![CI](https://github.com/dxn111/EVOLV/actions/workflows/ci.yml/badge.svg)](https://github.com/dxn111/EVOLV/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-8b93ab?style=flat-square)](LICENSE)
[![Release](https://img.shields.io/github/v/release/dxn111/EVOLV?style=flat-square&color=22d3ee)](https://github.com/dxn111/EVOLV/releases)

**No API key. No signup. One command.**

```bash
curl -fsSL https://raw.githubusercontent.com/dxn111/EVOLV/main/install.sh | sh
```

```bash
$ evolv          # boots on localhost:7373, opens your browser
```

<img src="docs/assets/demo.gif" alt="EVOLV live demo: the AI grows a pomodoro timer, a prompt library and a playable Flappy Bird directly into its own page — inline in the conversation, live, no refresh" width="88%">

[▶ **watch the full video (mp4)**](docs/assets/demo.mp4) — no edits, real stream

</div>

---

## What you just watched

That GIF wasn't scripted. You're looking at an AI **rewriting its own application while it runs**:

1. A human asked for a pomodoro timer.
2. The agent didn't paste code back. It **wrote a real component into its own source tree** (`~/.evolv/workspace/ui/components/pomodoro.html`) and logged the change in an append-only evolution ledger.
3. A filesystem watcher caught the write the instant it happened and pushed it over WebSocket — the new panel appeared **in the conversation itself, with no refresh, no rebuild, no restart**.
4. Then it grew a playable **Flappy Bird** the same way. The whole page is the canvas — every component mounts inline where it was born, and any organ can be opened **across the entire window** from the sidebar.

The app you installed is not the app you're running five minutes later.

> Most "AI workspaces" give you a chat window with plugins someone else wrote.
> EVOLV *is* the chat window — and it writes its own plugins, its own UI, and its own mind.

<div align="center"><img src="docs/assets/shot_wave.png" alt="EVOLV workspace: a pomodoro organ mounted inline in the conversation, with the organs list, Mind skills and evolution ledger in the sidebar" width="92%"></div>

### Part 2: it edits its own codebase — and you see it in real time

Components are just the surface. The second demo asks the organism to **rewrite its own source**:

<div align="center"><img src="docs/assets/selfedit.gif" alt="EVOLV edits its own index.html and style.css live — the RECOMPILING SHELL overlay appears, the accent color changes, the footer text changes, all mid-conversation" width="88%"></div>

[▶ **watch the full video (mp4)**](docs/assets/selfedit.mp4) — the agent edits its own `index.html` and `style.css`, forces its own recompile (watch the RECOMPILING SHELL overlay), and keeps going. In the same session it also:

- **grew a Prompt Library button** — template cards that drop into the composer — plus the skill for using it (one batch, two mutations);
- **installed `ghost-log`, a live plugin** in its own sandbox that pops a toast every time the organism mutates;
- **grew a playable Flappy Bird** onto its canvas. A video game, written by the app into itself, live:

<div align="center"><img src="docs/assets/shot_flappy.png" alt="EVOLV grew a playable Flappy Bird inline in its conversation, next to the evolution ledger and the organs list — the whole page is the canvas" width="92%"></div>

And the overlay shot — the app flashing "RECOMPILING SHELL · agent requested a shell recompile" *because the agent asked it to*:

<div align="center"><img src="docs/assets/shot_selfedit.png" alt="The RECOMPILING SHELL overlay mid-demo: the agent requested a recompile of its own shell after editing its own index.html and style.css" width="92%"></div>

<details>
<summary><b>More views of the organism</b></summary>

<div align="center">
<img src="docs/assets/shot_welcome.png" alt="EVOLV welcome screen — quiet dark shell with suggestion cards" width="92%">
<br><em>the shell at boot: quiet, warm, no neon</em>
<br><br>
<img src="docs/assets/shot_genome.png" alt="EVOLV genome inspector: every file of the agent's body, editable live" width="92%">
<br><em>the genome inspector: every file of the body, editable live</em>
</div>

</details>

---

## Why this is different

| | Traditional AI chat | Plugin-based workspace | **EVOLV** |
|---|---|---|---|
| UI | fixed | fixed | **self-rewriting, hot-reloaded** |
| Tools | prebuilt | marketplace | **grown at runtime, compiled live** |
| Skills / prompts | frozen | frozen | **self-edited, versioned in a ledger** |
| New feature request | "here's some code you could paste…" | "wait for the dev" | **materialized on your canvas in seconds** |
| Improvement loop | ends when the chat ends | manual | **continuous, visible, alive** |

### It edits itself. It edits YOUR code.

EVOLV is not a walled-garden canvas. It is a **full agent workspace** with the whole machine as its hands:

- **Own body, every byte.** Its chat shell, its CSS skin, its tool call cards, its front-brain JS, its panels — all plain files it rewrites live. Say *"make your tool call cards look exactly like Claude's"* and it opens its own `style.css`, rewrites the card section, reloads its shell — and the new skin lands in under a second, no refresh.
- **Even the launcher.** If the EVOLV source tree is on the machine, it detects it and can read and edit its own Go kernel (`cmd/`, `internal/`) — the code that runs it. Then `go build ./cmd/evolv`. A program that patches its own bootloader.
- **Your projects.** Point it at any folder (*"work on the project at ~/my-app"*, or `set_project`): it reads, edits, runs tests and ships code there like any serious coding agent — while still being the workspace it lives in.

### Zero refresh. Ever. You see every change as it happens.

- **Skin hot-swap** — when the agent (or you) rewrites `style.css`, the new skin slides in **without a reload and without a flash**. The organism changes clothes while you're looking at it.
- **Genome Inspector** — the 🗂 button opens every file of the body in a live modal: watch the file list flicker as the agent mutates (changed files flash green), open anything, edit it, hit ctrl+s. Your write lands in the genome like one of the agent's own — same watcher, same live effect.
- **The Mind panel** — the sidebar shows every skill, tool, plugin and prompt the organism currently carries, updating the instant something grows.
- **Shell recompiles** (index.html / app.js) still show the "RECOMPILING SHELL" overlay — a 900ms morph, not a reload.

### A body that doesn't embarrass itself

The default skin is a quiet, Claude-grade warm dark theme — no neon cyberpunk, no particle noise, no emoji confetti. Chat looks like chat: your messages right-aligned in soft bubbles, the agent's answers as clean prose, tool calls as calm bordered cards with a single green check. There is no separate "canvas pane" — **the whole page is the canvas**: components the organism grows mount inline in the conversation, and the ⤢ control (or the Organs list in the sidebar) opens any organ across the entire window. And since the skin is just `workspace/ui/style.css`, the organism can still restyle every pixel of this while it runs — that's the point.

### Plugins that are actually alive

Plugins aren't dead scripts — each one runs in **its own sandbox with a real-time nervous system**:

```js
// workspace/plugins/<name>/plugin.js — this is running, now
evolv.on("evolution", (e) => evolv.log("saw a mutation:", e.kind, e.name));
evolv.on("agent:token", (e) => { /* react to every streamed token */ });
evolv.every(30000, () => evolv.log("heartbeat — still alive"));
evolv.toast("hello from inside the organism");
```

`evolv.on` fires the moment the event happens on the bus — mutations, streamed tokens, phase changes, fs changes, dreams. The repo ships a `pulse` plugin so you can watch it work immediately.

### Time travel: every state of the organism is kept

Before **every mutation batch** the whole genome is frozen into `~/.evolv/snapshots/` (the last 20 states are kept). The agent can freeze and rewind itself mid-conversation:

```
{"action":"snapshot"}                       → "genome frozen — restore anytime"
{"action":"rollback","path":"latest"}       → the whole organism rewinds, live
```

Rolled back a bad evolution and instantly regretted it? Snapshots are also exposed in the UI/API (`/api/snapshots`), so *you* can time-travel the organism too.

### Dream mode: it evolves itself while you're away

Flip on **Dream mode** in Settings (or press the *💭 dream now* button). When the organism has been idle for the heartbeat interval, it wakes up **on its own**, reviews its own body — skills, prompts, tools, its own skin — grows one real improvement, and rolls over back to sleep. You come back to a chat that quietly says:

> 💭 dreamed while you were away. I taught myself deep-work.

Every dream is a real conversation turn, so the organism *remembers its dreams* — and every dream is snapshotted first, so nightmares are one rollback away.

### Swarm mode: many minds, one organism

Open **`/swarm`** (the 🐝 button in the sidebar) to convene a colony of **5–100 agents** across five roles: **Ideators** propose directions, **Coders** build the chosen one, **Testers** try to break it, **Critics** argue with both, and **Judges** write the final decision. They share one transcript, work in phases, and settle decisions with explicit **polls** — every agent casts a vote with a reason. Votes are collected in one call per role cohort, so a 100-agent poll costs five model calls instead of a hundred.

If the ship poll says **revise**, the coders apply the critique and the testers retest before the judge closes the run. The page streams live `swarm:activity`, `swarm:state` and poll bars over the existing WebSocket, filters the roster by role, exports the whole run as Markdown, and restores the last run from `workspace/swarm/last.json` after a restart. Terminal runs are archived under `workspace/swarm/runs/` and reopen from the run picker.

You are in the room too: **type while the colony is working to steer it**, and **click vote on an open poll** to add your own ballot. A live colony map and phase rail show where the 5–100 agents are. `POST /api/swarm` starts a run (`agents` 5–100); `POST /api/swarm/steer` adds a human message; `POST /api/swarm/vote` casts your vote; `/api/swarm/stop` cancels. Beta, and honest about it: artifacts are proposals until a tester actually runs them.

### The full action list

| Action | What it does |
|---|---|
| `set_plan` / `plan_advance` / `complete_task` | persistent task state, one-call progress, and a structured completion report |
| `evolve_component` | mounts a new live UI panel mid-conversation |
| `evolve_tool` / `run_tool` / `list_tools` | compiles a JS tool into its sandbox, with an optional typed argument schema |
| `evolve_skill` / `evolve_prompt` | grows its own playbooks and system prompt |
| `evolve_plugin` | installs a **live** plugin: own sandbox, reacts to bus events via `evolv.on`, real timers via `evolv.every` |
| `write_file` / `edit_file` / `read_file` | any file, any path on your machine (hash-guarded writes) |
| `list_dir` / `tree` / `map_repo` / `move` / `delete_file` | filesystem navigation plus a cached Go/JS/TS repository outline |
| `run_tests` / `set_project` / `shell` | auto-detected test suites, project binding, commands (120s timeout) |
| `snapshot` / `rollback` | time travel: freeze the genome, rewind the whole organism |
| `stage_candidate` / `list_candidates` / `promote_candidate` | isolated, hash-pinned kernel proposals with an explicit promotion gate |
| `self_modify` / `self_update` | compile, test and stage a kernel candidate, or morph to the latest release |
| `reload_shell` | forces the browser to recompile its UI now |

Every mutation is logged to `workspace/evolution.jsonl` and streamed to your screen as it happens. And you can play too: **the body is plain files.** Edit anything in `~/.evolv/workspace/` — it hot-reloads.

---

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/dxn111/EVOLV/main/install.sh | sh
```

The installer detects your OS/arch, downloads the prebuilt binary (or builds from source if needed), puts `evolv` on your PATH, and prints a launch card. Check what it would install first with `install.sh --version`, or pin a release with `EVOLV_VERSION=v0.22.0 install.sh`.

**Full documentation:** [docs/index.html](docs/index.html) — a single-page guide generated straight from this README (`go run ./cmd/evolv-docs`), plus the [24-hour master plan](docs/EVOLV-24H-MASTERPLAN.md).

<details>
<summary><b>Other ways in</b></summary>

```bash
# with Go toolchain
go install github.com/dxn111/EVOLV/evolv@latest

# from source
git clone https://github.com/dxn111/EVOLV
cd EVOLV && make install

# grab a binary from Releases
# macOS / Linux / Windows (amd64 + arm64) — see github.com/dxn111/EVOLV/releases
```
</details>

Then:

```bash
evolv                  # → http://localhost:7373 opens in your browser automatically
evolv update           # morph in-place to the latest release (genome survives)
evolv update --check   # current vs latest, no download
evolv download         # fetch the latest platform binary into this folder
evolv status           # live health card for a running organism
evolv --port 8080      # custom port
evolv --no-open        # headless / ssh
evolv --reset          # restore the factory genome (your evolution ledger + snapshots survive)
evolv help             # the full command map
```

<div align="center"><img src="docs/assets/shot_settings.png" alt="EVOLV settings drawer: provider picker with free anonymous Kilo Gateway, BYOK fields for OpenAI, Anthropic, OpenRouter, Groq, Ollama and custom endpoints" width="92%"></div>

---

## Models: free by default, BYOK when you want power

EVOLV ships with the **[Kilo Gateway](https://kilo.ai/gateway)** as its default brain — **anonymous, IP-based free access, zero signup** (200 requests/hour). You download, it thinks.

| Provider | Key needed | Notes |
|---|---|---|
| **Kilo Gateway** *(default)* | none | `kilo-auto/free` rotates the live free-model pool for you |
| Kilo Gateway + BYOK | kilo | unlocks `kilo-auto/frontier`, Claude, GPT, GLM, Kimi, Qwen, Grok, DeepSeek… |
| OpenAI | openai | any OpenAI-compatible endpoint via custom base URL |
| Anthropic | anthropic | native Messages API |
| OpenRouter | openrouter | hundreds of models, one key |
| Groq | groq | absurdly fast |
| Ollama | none (local) | your machine, your weights, your rules |
| Custom | custom | any OpenAI-compatible URL |
| Mock | none | offline evolution demo — mutates with no brain at all |

Change it any time in **Settings → Provider** — the swap is hot, mid-conversation.

> Free anonymous models are small and inconsistent. Great for watching evolution happen; plug in a frontier model via BYOK when you want serious work.

---

## The evolv protocol

Self-modification works on **every model** (even tiny free ones) because it's a plain-text protocol, not a vendor-specific function-calling API. The agent emits:

````markdown
```evolv
{"action":"evolve_component","name":"clock","title":"Clock","icon":"🕰","description":"A live clock","html":"<div id='c-clock'>…</div><script>…</script>"}
{"action":"evolve_tool","name":"dice","description":"Roll a die","code":"function dice(args){return {roll:Math.ceil(Math.random()*((args&&args.sides)||6))}}"}
```
````

The kernel parses it out of the stream, executes each action against the living filesystem, broadcasts the mutation to every connected browser, feeds the results back to the model, and loops — so the agent can **verify its own work and continue**.

And because small free models are sloppy, the parser is deliberately forgiving — the model is never punished for a broken fence with a dropped mutation:

- a **fence left unterminated** (the model streams past the closing ` ``` `) still executes;
- **raw `{"action":…}` JSON leaked into chat prose** — no fence at all — is salvaged and executed, and the UI renders it as a clean tool card instead of garbage;
- objects separated by `}{`, trailing junk, braces inside strings — all handled by a string-aware brace scanner;
- the prompt hard-chains the loop: *read → edit → test → next batch*, never one tool call and a shrug.

### Genome layout

```
~/.evolv/
├── config.json                  # kernel config (provider, keys, project root, port…)
├── logs/
└── workspace/
    ├── ui/                      # the ENTIRE frontend — served & hot-reloaded
    │   ├── index.html           #   the agent can rewrite its own shell
    │   ├── app.js               #   (you'll see a "genome recompiling" flash)
    │   ├── style.css            #   …and its own CSS
    │   └── components/          # hot-mounted panels (one .html file each)
    ├── tools/                   # JS tools the agent grew (goja sandbox)
    ├── skills/                  # markdown skills → part of its mind
    ├── prompts/                 # markdown fragments → appended to its system prompt
    ├── plugins/                 # plugin.json + plugin.js, sandboxed
    ├── evolution.jsonl          # append-only ledger of every mutation, ever
    └── ../snapshots/            # frozen genome states (auto, before every mutation batch)
```

> Wait — `snapshots/` sits at `~/.evolv/snapshots/`, one level above `workspace/`. It holds the last 20 complete genome states, newest first.

### The skills it boots with

Skills are markdown playbooks, indexed in the system prompt with a one-line trigger and loaded only when the match fires. A newborn organism ships with:

| Skill | When it fires |
|---|---|
| `systematic-debugging` | a build, test, or runtime failure with no obvious cause |
| `test-driven-change` | fixing a bug or adding behavior that can be checked |
| `self-evolution-playbook` | rewriting its own UI, tools, prompts, plugins, or kernel |
| `frontend-craft` | growing a component or restyling the shell |
| `repo-cartography` | entering an unfamiliar repo or planning a wide change |
| `secure-by-default` | credentials, shell, plugins, paths, or network access |
| `workspace-steward` | deciding what earns space on the canvas |

Skills are yours too: add a `.md` under `~/.evolv/workspace/skills/` (optional YAML frontmatter with `name` + `description`) and the next turn can find it. The organism can also grow one for you with `evolve_skill`.

### Under the hood

```
┌────────────┐   chat    ┌───────────────┐   evolv blocks   ┌──────────────────┐
│  Browser   │◄─────────►│  evolv kernel  │◄────────────────►│  LLM (Kilo/BYOK)  │
│ (canvas +  │           │  agent loop    │    plain text     └──────────────────┘
│  stream)   │  WebSocket│  + goja sandbox│
└────────────┘    ▲      └───────┬────────┘
                  │              │ writes
                  │ mutations    ▼
                  │      ┌───────────────┐
                  └──────│  file watcher │  ~/.evolv/workspace/**
                         └───────────────┘  (150ms poll, instant feel)
```

- **Single static binary** (Go + embedded genome) — no runtime deps, no node_modules, no docker
- **goja** (pure-Go JS interpreter) executes the tools/plugins the agent writes — no cgo, cross-compiles to every platform
- **150ms filesystem watcher** + WebSocket fan-out = "did it just…? yes it did"
- **Per-plugin goja sandboxes** with a live event bus (`evolv.on` / `evolv.every`) — plugins react while the organism thinks
- **Operational surface** — `GET /api/health` reports uptime, provider/model, model calls/errors/latency, request/error counts, live sockets and snapshots; `GET /api/metrics` exposes counters plus a request-latency histogram in Prometheus format; `GET /api/history?limit=&offset=` pages the active transcript
- **Local-only guards** — loopback Host validation, same-origin Origin checks for HTTP and WebSocket, token-bucket rate limits, 64 KiB WebSocket frames and ping/pong read deadlines
- **Shutdown that behaves** — HTTP header/read/idle timeouts, bounded request bodies, panic recovery, and a SIGTERM drain that closes live sockets before exiting
- Everything local. Nothing phones home except the model provider you choose.

---

## It updates itself too

The kernel polls GitHub Releases at boot and every 6 hours. When a newer version exists, a **live banner drops into every open browser** — a program telling you it can become a better version of itself, mid-session:

> 🧬 a newer organism exists: v0.3.0 — **[morph now]**

Click *morph now* (or run `evolv update`) and the latest binary is downloaded and swapped over the running one. Your genome — every evolution it ever made — survives untouched. Restart once and the new body is live.

Downloads are **SHA-256 verified against the release's `checksums.txt`** before a binary can be swapped. `evolv download` gives you the same verified artifact without touching the launcher.

---

## Security (read this)

EVOLV is a self-modifying agent running on **your** machine — that's the entire point, and it comes with real teeth:

- The agent can run **shell commands** and read/write **any file on the machine** — that's what makes it a real workspace, not a toy. It binds to `127.0.0.1`, and the browser UI only exposes what the kernel serves.
- Evolved components and tools are **security-linted before mounting**: external script injection, remote exfiltration calls, cookie access, and dangerous URI schemes are automatically detected and blocked to ensure components remain strictly local and safe.
- Free anonymous models may route through providers that log prompts — **don't paste secrets into demo-tier chats.**
- `evolv --reset` restores the factory genome if an evolution goes feral.

Treat it like a very eager intern with root: brilliant, fun, supervise accordingly.

---

## Roadmap

- [x] Ambient heartbeat — **shipped in v0.4.0 as Dream Mode** (idle → autonomous self-evolution)
- [x] Evolution branching — **shipped in v0.4.0 as snapshots + rollback** (time travel through genome states)
- [x] Memory that survives restarts — **shipped in v0.7.0** (conversation persisted atomically)
- [x] Network reach — **shipped in v0.7.0** (`evolv.http()` inside tools and plugins)
- [x] The whole page is the canvas — **shipped in v0.10.0** (organs mount inline in the conversation, open full-window from the sidebar)
- [x] A shell that doesn't embarrass itself — **shipped in v0.8.0** (markdown rendering, copy buttons, streaming caret, welcome screen, genome file deletion)
- [x] Component marketplace — export / import genomes as `.evolvpack` files (CLI and web API)
- [x] Swarm mode — 5–100 role-specialised agents think, build, test and vote together; live beta at `/swarm`
- [ ] Vision — components that can see (canvas screenshots as tool input)
- [ ] Voice

## Changelog (the organism's fossil record)

| Version | What evolved |
|---|---|
| **v0.30.0** | **the mind gets a playbook, the pipeline gets a gate** — the system prompt is now an operating manual: frame → plan → inspect → mutate → verify → report, with exact action signatures, stale-write discipline, secret and destructive-command rails, kernel-surgery rules and a definition of done; seven seeded skills are discovered through a compact trigger index (systematic debugging, test-driven change, self-evolution, frontend craft, repo cartography, secure-by-default, canvas stewardship) and load only when they match; **bug fixes**: `move {from,to}` parses again, `complete_task.files` accepts path lists instead of failing at decode, empty/root `delete_file` can no longer wipe the organism, plugin reload/re-evolve retires old sandboxes instead of leaking timers, broken `evolve_tool` JavaScript is reported instead of silently "compiling", tool schema validation is deterministic, and `promote_candidate` requires explicit confirmation; **CI workflow** (gofmt + vet + tests + race + cross-build) and release builds now use `go.mod`'s Go version and test before publishing; includes the DNA workspace — DNA Builder strands, Creation Library, live pinned widgets, enable/archive controls, SHA-256 stale-write guards and isolated candidate evolution that must test/build green before explicit promotion |
| **v0.29.0** | **search the whole project from the Workbench** — new `⌕ search` box greps every text file under the project root (case-insensitive, `.git`/`node_modules`/… respected, capped at 200 hits with a "showing first 200" hint, per-match file:line + context); click a result and the editor **opens the file with the caret exactly on the match line, selection highlighting it**; new `/api/wb/search` endpoint + test; **verified live: 14/14 browser E2E** |
| **v0.28.0** | **the Workbench — a real dev environment for your project** — new `🛠 Workbench` opens the bound project (config.project_dir, or organism home): lazy file tree with type icons + sizes and skip-list (`.git`, `node_modules`, …), in-place text editor with **ctrl+s write-to-disk**, new file/dir, rename/move, delete, live refresh (the tree updates when anything else touches the project); behind it a full fs API (`/api/wb/{root,tree,read,write,mkdir,delete,rename}`) with **path-escape + symlink-escape hardening**, 512 KB read / 1 MB write caps and polite binary refusal; **BUG FIX: the recompile loop** — shell-reload storms (post-update seeding, dream bursts, rollbacks) could bounce the page on "recompiling shell" forever; a sessionStorage loop guard now reloads exactly once per storm; 8 new server tests + **verified live: 12/12 browser E2E** (tree → open → edit → ctrl+s → disk → rename → delete round-trip) |
| **v0.27.0** | **plugins run in cages now** — `evolve_plugin` accepts explicit `perms:["http","fs","toast"]`; a plugin without a permission gets a **catchable error naming the missing capability** (`plugin "x" lacks permission "http" — re-evolve it with perms:["http"]`) instead of silently reaching the real world; perms persist in `plugin.json` and are restored on every reload; `log`/`on`/`every` stay core (always on); 4 new tests (denial matrix, grant path, manifest persist + reload round-trip, core-always-allowed) — full suite + `-race` green |
| **v0.26.0** | **tools grow type systems** — `evolve_tool` accepts an optional typed contract (`schema: {param: "number" / "string" / "boolean" / "object" / "array" / "any"}`), persisted as a **versioned manifest** (`workspace/tools/<name>.json`, version auto-increments on re-evolve) and **enforced by `run_tool`**: bad calls fail with a precise type error (`arg "a" must be number, got string`) instead of a JS stack trace; new `list_tools` action gives the brain full introspection (name, description, version, schema); manifest-less legacy tools keep working; 5 new tests (manifest persistence + version bump, validation matrix, back-compat, introspection, bad-type rejection) — all green |
| **v0.25.1** | **the demo brain works its plan the new way** — the mock planner demo now walks declare → `plan_advance` → `plan_advance`×2 (one call = one step of progress) instead of re-sending the whole `set_plan` every turn; scenario test rewritten to assert exact advance-block counts; **verified live: 9/9 browser E2E** (boot 1/4 → live march 2/4 → 3/4 → 4/4 with correct statuses each step) |
| **v0.25.0** | **the agent works its own plan** — new `plan_advance` action: one call marks the current step done (by index or the doing step) and pulls the next todo into doing, so the brain loops through multi-step work without re-sending the whole plan; the declared plan is now **injected into the system prompt every turn** (`## CURRENT PLAN` with [x]/[~]/[ ] markers), so the model always knows where it is in its own work; plan helpers factored (`publishPlan`/`countDone`); 4 new tests (advance chain, index advance, bogus rejection, prompt injection) — all green |
| **v0.24.0** | **the agent keeps a plan** — first slice of the P0 planner: a new `set_plan` action lets the agent declare and advance multi-step task state (`{title,status}` per step, normalized to todo/doing/done), persisted atomically to `workspace/plan.json` (survives restarts), served at `/api/plan`, broadcast live over the bus, and rendered as a sidebar Plan panel with progress counter and ☑/◐/○ statuses (boot-restored on every load); the demo brain walks a plan declare → progress → done on request |
| **v0.22.0** | **the docs evolve too** — a single-page docs site generated straight from the README (`internal/docsgen`: section extraction + pragmatic markdown renderer with tables/fences/lists, HTML-escaped, sticky-TOC dark page; `go run ./cmd/evolv-docs` regenerates `docs/index.html` — 13 sections today), the installer grows `--version` / `--help` flags (resolves and prints the real latest release before you pipe it into sh), and the sprint gets a real engineering master plan at `docs/EVOLV-24H-MASTERPLAN.md` (repo audit, scored priorities, security backlog) |
| **v0.21.0** | **fast while it sleeps** — performance pass for organisms that mutate all night: the evolution ledger is virtualized (DOM holds the newest 200 entries, everything older stays out of the render tree behind a "⏵ show N older mutations" pager — the sidebar stays smooth at 1,000+ mutations), and streaming markdown is rAF-batched (token bursts coalesce into one parse + DOM rebuild per animation frame instead of dozens per second) |
| **v0.20.0** | **dreams that write their own code** — the idle dream engine is no longer a set of canned widgets: every dream composes a brand-new generative organ (palette, motion family, density and name drawn at random — orbs/waves/drift/rings/lines), and the other dreams re-open the previous organ's own file at `ui/components/<name>.html`, rewrite its code and let the shell remount it live. Plus the dream log: dream mutations are tagged `dream:true` in the evolution ledger, the sidebar groups consecutive dream mutations under a 🌙 "dream — nobody watching" divider, and the welcome screen greets you with "it dreamt N times while you were away" (click → replay) |
| **v0.19.5** | **genome inspector, upgraded** — file-type icons across the whole tree (🧩 organs, ⚙️ tools, 📝 skills, 🎨 skin, 🔌 plugins), a fuzzy filter box (subsequence match with highlighted glyphs, `n of m files` counter, Esc clears), unsaved-edit protection (switch/close warns, path shows ● unsaved) and a one-click ↺ revert back to the genome's own version |
| **v0.19.0** | **autonomic nervous system** — the demo organism now evolves BY ITSELF: idle heartbeat dreams grow real organs with zero keywords (music-box synth, gravity-pool physics, presence-clock, hex-pulse generative art), one per dream, then quiet — verified live, one mutation per minute with nobody typing |
| **v0.18.0** | **🐍 playable snake** — say "snake" and the mock brain grows a full arcade into its own canvas: 21×21 grid, arrows/WASD + swipe controls, score/best/length HUD, speed ramps every 3 foods, persistent best score; "snake" correctly outranks the generic "game"→flappy mapping |
| **v0.17.0** | **share this conversation** — ⇪ in the header downloads the whole transcript as one standalone HTML file (dark branded layout, XSS-escaped, zero external assets) via the new `/api/export` route; shell v15 |
| **v0.16.0** | **conversation search** — ctrl+f opens a search bar over the chat: live match highlighting (CSS Custom Highlight API, zero DOM mutation), result count, enter/shift+enter to jump between matches, esc to close; shell v14 |
| **v0.15.0** | **sound & soul** — 🔊 in the header wakes tiny WebAudio blips: a soft sine on file change, a triangle chirp on every mutation, a send blip; off by default, remembered per browser; shell v13 |
| **v0.14.0** | **mobile pass** — EVOLV fits in your pocket: sidebar folds away on <820px and opens over a dimmed tap-to-close backdrop, full-bleed modals (organ / genome / diff / replay fill the screen), composer respects the home-indicator safe area, tested live at 390×844; shell v12 |
| **v0.13.0** | **"what changed" genome diff** — ⏪ in the genome inspector diffs any two snapshots line-by-line (real LCS diff engine, `/api/snapshots/diff`); along the way: fixed same-second snapshot ID collisions and hardened snapshot restore against path escape — both caught by new tests; shell v11 |
| **v0.12.0** | **replay timeline** — ⏳ in the sidebar opens the organism's entire fossil record (`/api/replay`, every mutation birth→now on one spine, color-coded by kind, click any entry to jump to the file it grew in the genome inspector); shell v10 |
| **v0.11.0** | **stop button** (abort the organism mid-thought — real context cancel, not a cosmetic pause), **daylight "paper" theme** (one click / ctrl+shift+l, persisted), keyboard shortcuts (ctrl+k new chat · ctrl+b sidebar · / focus · esc closes), draggable resizable sidebar, shell v9 |
| **v0.10.0** | the right-hand canvas pane is dead — **the whole page is the canvas**: organs mount inline in the conversation, sidebar Organs list, full-window organ mode; demo brain grows Flappy Bird + prompt-library + plugins + live self-edit |
| **v0.9.0** | demo brain grows up: the organism self-edits its own shell live (RECOMPILING SHELL), grows prompt-library buttons + skills in one batch, installs reactive plugins, and grows a playable Flappy Bird into its canvas; PayPal funding link |
| **v0.8.0** | markdown-lite v2 (lists, quotes, links), code blocks with language bar + copy, hover-to-copy messages, streaming caret, welcome screen, genome inspector can delete files — shell bumped to v7 for existing genomes |
| **v0.7.0** | conversation persists across restarts (atomic writes, corrupt-tolerant), `evolv.http()` for tools **and** plugins, silent provider retry |
| **v0.6.0** | batched multi-tool turns, real-time tool-card states, forgiving parser (salvaged raw JSON, unterminated fences), Claude-grade default skin, versioned shell reseed |
| **v0.5.0** | genome inspector, per-plugin sandboxes, skin hot-swap, mind panel |
| **v0.4.0** | dream mode, snapshots + rollback |

## FAQ

<details>
<summary><b>Is this just Electron + eval?</b></summary>
No. It's one Go binary serving its own genome; the "eval" is a properly sandboxed goja runtime for agent-written tools, and UI components are plain files you can audit, edit, or delete at any time.
</details>

<details>
<summary><b>What if the AI breaks its own UI?</b></summary>
It happens sometimes — that's evolution. The factory genome is embedded in the binary; <code>evolv --reset</code> instantly restores it while keeping your evolution ledger. You can also fix it yourself: the UI is plain files.
</details>

<details>
<summary><b>Does it need the internet?</b></summary>
Only for models. With Ollama you get a fully local, offline self-evolving workspace. With the mock provider it evolves with zero brain at all (great for screenshots and parties).
</details>

<details>
<summary><b>Why "EVOLV"?</b></summary>
Because it does. Literally. You watched it happen in the GIF.
</details>

---

## Contribute

The most fun repo on your machine to hack on:

```bash
git clone https://github.com/dxn111/EVOLV && cd EVOLV
make build && ./evolv
```

Before opening a PR, run the same gate CI runs:

```bash
make check     # gofmt check + go vet + go test ./...
make test      # tests only
make cross     # verify every release target still builds
```

CI (`.github/workflows/ci.yml`) runs formatting, tidy/docs checks, tests + coverage, a real-server boot/SIGTERM smoke, the full-module race detector, Go version compatibility, and cross-builds Linux/macOS/Windows on every push and pull request. Releases are validated with the latest patched Go 1.26.x, smoke-tested, checksummed and provenance-attested before the binaries are published.

Ideas that would be amazing: new protocol actions, a genome inspector UI, evolution replay/branching, ports to new platforms. Open an issue or ping me directly — **I answer everything.**

## Support & ideas

- **dxn1.0day@gmail.com** · Discord **@dxn1_ai**

If EVOLV made you say "wait, WHAT?" — a star genuinely helps other people find it. And if you want to help the mutation spread: [tweet about it](https://twitter.com/intent/tweet?text=EVOLV%20is%20an%20AI%20workspace%20that%20rewrites%20its%20own%20UI%2C%20tools%20and%20prompts%20while%20it%20runs.%20Watch%20it%20happen%3A&url=https%3A%2F%2Fgithub.com%2Fdxn111%2FEVOLV), post it on r/LocalLLaMA, or send the GIF to one friend who won't believe it.

<details>
<summary><b>About / keywords</b></summary>
EVOLV — self-evolving AI workspace. AI that rewrites its own UI, tools and prompts live. A self-modifying agent and full coding workspace that works on your projects. Local-first, single Go binary, BYOK, free AI models via the Kilo Gateway. Think opencode or cursor, but the app itself is the thing that evolves.
</details>

<div align="center">

[![Star History Chart](https://api.star-history.com/svg?repos=dxn111/EVOLV&type=Date)](https://star-history.com/#dxn111/EVOLV&Date)

</div>

## License

MIT © [dxn111](https://github.com/dxn111)
