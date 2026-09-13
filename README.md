# Marl

**Multi-agent task runner in a single binary.** Give it a task; it forks
sub-agents to work in parallel, keeps every agent inside a sandbox, shows
you every agent in a live tree, and stops to ask you when something needs
a human decision — all from a terminal dashboard.

[中文文档](README.zh-CN.md)

## Install

One command (installs `fossil` + `marl`; optionally the `sqlite3` CLI):

```bash
curl -fsSL https://raw.githubusercontent.com/RobiNexy/Marl/main/scripts/install.sh | sh
```

The script detects your platform — Linux (apt/dnf/pacman/zypper or static
fossil download), macOS (brew), and **Termux/Android** (`pkg`, arm64
binaries) — verifies sha256 checksums, and finishes with `marl doctor`.

From source (Go 1.27+, no CGO needed):

```bash
git clone https://github.com/RobiNexy/Marl && cd Marl
go install ./cmd/marl
```

Requirements: [fossil](https://fossil-scm.org) 2.20+ on PATH and a
DeepSeek API key (`DEEPSEEK_API_KEY`). Check everything:

```bash
marl doctor
```

## 30-second quick start

```bash
marl init ~/my-task                       # one-time: create a project
cd ~/my-task
export DEEPSEEK_API_KEY=sk-...

marl tui                                  # open the dashboard
# press: /start "Read README.md and summarize it"
```

Prefer the classic CLI? `marl start "…"` runs attached, `marl start
--detach "…"` runs in the background. Both use the same engine as the TUI.

## The TUI: your AI team's cockpit

`marl tui` is a terminal dashboard (bubbletea). It connects to a running
daemon if there is one, or runs the engine in-process — you don't have to
care which.

```
┌─────────────────────────────────────────────────────────────────────┐
│ Marl · 任务运行中 · 成本 ¥0.0428 CNY · 12.3k tok · 缓存 62% · [in-process] │
├──────────────┬──────────────────────────────────┬───────────────────┤
│ 监督树        │ 对话 · sub_000001                │ 成本与告警         │
│ 👤 human      │ 你                               │ ¥0.0428 CNY       │
│ └🤖 sub_001 ●│  读一下 README                   │ tokens 12.3k      │
│    ├🔧 sub_002│ sub_000001                      │ 思维链 18% 缓存 62%│
│    └🔧 sub_003│  好的，我来读取…                 │ r0 flash ¥0.006   │
├──────────────┴──────────────────────────────────┴───────────────────┤
│ ⚡ 待你决策 (1)：[gate] rm -rf build —— 按 a 处理                     │
├─────────────────────────────────────────────────────────────────────┤
│ 到 sub_000001 ▸ 发消息给选中 agent…                                  │
├─────────────────────────────────────────────────────────────────────┤
│ a 审批 · ↑↓ 选 agent · i 输入 · f 跟随 · q 退出(任务保持)            │
└─────────────────────────────────────────────────────────────────────┘
```

**Key bindings** (also under `?`):

| Key | Action |
| :-- | :-- |
| `a` | **Action Center** — everything waiting for your decision |
| `↑↓` / `Enter` | Select an agent in the tree / open its conversation |
| `i` / `/` | Type a message / slash command (`Esc` back to panels) |
| `Tab` `1/2/3` | Cycle focus / jump to tree · workspace · sidebar |
| `t` `f` `s` | Fold thinking · toggle auto-follow · collapse sidebar |
| `g t/e/i` | Workspace tab: conversation / events / inspector |
| `q` | Quit (running task stays in the background) |

**Slash commands**: `/start <task>` · `/stop [force]` · `/say <agent>
<text>` · `/events` `/inspect` `/cost` · `/help` · `/quit`.

**Approving from the TUI.** Press `a`, pick a pending item, decide with
one key: `[o]` allow once · `[n]` allow next N · `[k]` grant tokens ·
`[a]` always allow · `[d]` deny. Structured replies are written to the
same inbox files you would edit by hand — and they take effect
**immediately** (they carry a "write-complete" marker; hand edits keep a
10 s quiescence window so half-typed answers are never read). The TUI has
no privileged backdoor: it is just a faster pen.

## The full lifecycle

```bash
marl init ~/my-task        # 1. create the project (fossil repo + .marl/)
marl doctor                # 2. verify fossil / API key / skeleton
marl tui                   # 3. open the dashboard
#    /start "…"           # 4. launch a task
#    watch the tree fork; approve gates with `a`
#    the report lands in your inbox + a completion notice
marl status                #    (CLI view of the same tree)
marl log -db .marl/store.db -out conv.md   # 5. export conversations
```

**Costs.** The sidebar shows total cost, tokens, reasoning share, cache
hit rate, and per-rung (model tier) breakdown. The engine climbs a model
ladder: cheap models first, upgrades only with evidence (repeated tool
format errors, no progress, ...). Every call is priced in
`.marl/store.db` (`ledger_entries`); reports also via
`go run ./cmd/ladder_report -db .marl/store.db`.

**Knowledge across projects.** Two levels:

| Level | Where | What it holds |
| :-- | :-- | :-- |
| Project | `.marl/knowledge/` (in-repo, versioned) | contracts, decisions, `preferences/` (compiled into every agent's fixed prompt) |
| **Global** | `~/.local/share/marl/global.fossil` (override: `XDG_DATA_HOME`) | knowledge you promote for reuse everywhere |

```bash
marl knowledge lint                  # preferences must fit the always-on budget
marl knowledge promote knowledge/decisions/api-style.md
marl knowledge pull                  # vendor global knowledge into this project
```

**Permanent approvals** live in the control plane too: `@always-grant`
writes a rule to `~/.local/state/marl/<project>/grants/` that survives
restarts.

## The three ideas behind Marl

**1. You are the root of the tree.**
Every project has one human (you). Agents you spawn form a tree below
you; any agent can hand work further down. Failures walk back up until
someone can act on them — that's you at the top.

**2. Tasks fork into parallel sub-tasks.**
An agent that splits its work forks child agents with `spawn_batch`; each
child gets an explicit write-scope (path globs) — a child physically
cannot write outside it. When a child finishes or crashes, its parent
receives a report; the framework reports on a child's behalf if it dies
silently.

**3. It stops and asks you.**
Expensive or risky operations pause the agent and drop a request into
your Action Center / inbox. Discussions work the same way: the agent
proposes, you annotate or approve, the outcome is committed under your
name. Everything is observable: append-only conversation logs, an audit
event stream, and a cost ledger. Agents can't fake approvals — approval
files live outside every agent's write scope.

## Version control: Marl uses fossil; your git is untouched

Marl's bookkeeping (knowledge, discussion drafts, agent work commits)
lives in a fossil repository at `.marl/project.fossil`, created by
`marl init`. **It is the only VCS Marl touches — there is no git
integration, and there are no plans to remove fossil.**

If your project is a git repository, the two coexist without interference:
fossil stores its data inside `.marl/` and never reads or writes `.git/`.
You keep committing to git as usual; Marl's agent commits (author =
`agent`) form a parallel history of machine work you can audit with
`fossil ui`. Think of it as Marl keeping receipts, not managing your code
branches.

## Inbox reference

Your inbox lives at `~/.local/state/marl/<project>/inbox/`
(override the root with `XDG_STATE_HOME`):

| File | Meaning | Your action |
| :-- | :-- | :-- |
| `report_*.md` | An agent's final report | read it |
| `gate_*.md` | Approval request | edit the `@grant …` line, save (or answer in the TUI) |
| `direct_*.md` | Message you sent (`marl say`) | — (auto-archived) |

Escalations (an agent stuck and asking you a question) land in
`~/.local/state/marl/<project>/requests/pending/`; write your reply under
`## 回复` and move the file to `done/` — or just answer from the TUI.

`@grant once` / `@grant next 20` / `@grant tokens 50000` / `@always-grant`
(permanent) / `@deny`.

## CLI reference

| Command | What it does |
| :-- | :-- |
| `marl tui` | Terminal dashboard (tree / approvals / costs / conversation) |
| `marl init [dir]` | Create a project (fossil repo + `.marl/` skeleton) |
| `marl start "task"` | Run a task (attached; Ctrl-C to interrupt) |
| `marl start --detach "task"` | Run in the background (log in the control plane) |
| `marl say -to <agent> "text"` | Message a running agent |
| `marl status` | Live supervision tree + blocked/waiting states |
| `marl stop [-force]` | Gracefully stop the background task |
| `marl serve [-addr 127.0.0.1:8731]` | Local service daemon: REST + SSE for GUIs |
| `marl log -db <db> [-out file.md]` | Export / print conversations |
| `marl attach -db <db>` | Tail new entries as they land |
| `marl knowledge lint` | Check the always-on knowledge block budget |
| `marl knowledge promote/pull` | Share knowledge across projects (fossil-based) |
| `marl models probe -models m1,m2` | Check a model: chat / tool-call / cache |
| `marl doctor` | Environment self-check |
| `marl version` | Build info |

## Configuration

All configuration is per-project YAML under `.marl/` (version-controlled
with your project):

| File | What it controls |
| :-- | :-- |
| `.marl/config.yaml` | Budget limits (per-task call counts, token caps, timeouts), approval rules, sidecar wires |
| `.marl/profiles/*.yaml` | Per-agent role: model capabilities, sampling params, skill whitelist, fork permission |
| `.marl/prompts/` | Prompt templates — editing a file changes that step's behavior, no code changes |
| `.marl/knowledge/` | Facts agents should always know; `preferences/` is written by you and compiled into every agent's fixed prompt |

Example — budget and approvals in `.marl/config.yaml`:

```yaml
project:
  max_depth: 3          # how deep agents may fork (human level excluded)
limits:
  llm_call:
    task_max_calls: 20  # over this → agent pauses and asks you
    task_max_tokens: 100000
gate_rules:
  - id: review-shell
    match: {kind: shell}
    action: need_human
    reason: "Only the go toolchain is pre-approved"
```

The YAML parser is intentionally strict: unknown keys and tab indentation
are errors, not silently-ignored typos.

## Building on it programmatically (Web UI / tools)

`marl serve` runs a local service daemon over one project — every TUI/GUI
operation is an HTTP call. Default address is loopback only.

```bash
DEEPSEEK_API_KEY=sk-... marl serve -dir ~/my-task -addr 127.0.0.1:8731
```

| Endpoint | What it does |
| :-- | :-- |
| `GET /api/v1/status` | Project, human, run state, agent tree |
| `POST /api/v1/tasks` `{"task": …}` | Start a task (409 if one is running) |
| `POST /api/v1/tasks/stop` | Stop the current task |
| `POST /api/v1/agents/{id}/messages` `{"text": …}` | Message a running agent |
| `GET /api/v1/inbox` · `GET /api/v1/inbox/{name}` | List / read inbox files |
| `POST /api/v1/inbox/gate/{id}` `{"action":"allow","mode":"count","count":20}` | Answer an approval |
| `GET /api/v1/escalations` · `POST /api/v1/escalations/{id}/reply` | List / answer escalations |
| `GET /api/v1/discussions` · `POST /api/v1/discussions/{id}/verdict` | List / reply to discussions |
| `GET /api/v1/events?since=<seq>` | SSE event stream (audit-backed, resumable) |
| `GET /api/v1/conversation?agent=…` · `/conversation/export` | Chat log / markdown export |
| `GET /api/v1/costs` | Cost report (tokens, cache hit, CNY) |
| `GET/PUT /api/v1/config` · `GET/PUT /api/v1/profiles/{id}` | Validated config editing (422 on mistakes) |
| `GET /api/v1/doctor` · `GET /api/v1/knowledge/lint` · `POST /api/v1/knowledge/promote|pull` | Diagnostics & knowledge |

Approvals via the API write the same inbox files a human would edit —
one mechanism, no privileged backdoor.

## FAQ / Troubleshooting

**Where is the global, cross-project configuration?** Three places, by
purpose: global *knowledge* at `~/.local/share/marl/global.fossil`
(`XDG_DATA_HOME` aware); per-project control plane (inbox, `grants/`,
discussions) at `~/.local/state/marl/<project>/`; per-project settings in
`.marl/` inside the repo.

**Does the agent manage my code with fossil?** Marl writes agent work
commits into `.marl/project.fossil` (its own receipts). Your git repo is
never touched — the two coexist. See the section above.

**How much did a run cost?** The TUI sidebar, or
`go run ./cmd/ladder_report -db .marl/store.db -task start-task`, or query
`.marl/store.db` (`ledger_entries`) directly.

**A sub-task failed. Is everything lost?** No. Failures are reported up
the tree, the root agent adapts or retries, and the full history stays in
the log.

**An agent is stuck "blocked".** It's waiting on a human file (approval,
discussion, escalation). Nothing is being spent while blocked; the
watchdog raises an alert if the wait stalls.

**My mid-run message didn't interrupt it.** Messages are injected between
rounds — they never interrupt work already in progress.

**Keys and secrets:** export `DEEPSEEK_API_KEY`; never commit it.

## For contributors

Design documents, architecture decisions (ADRs), and phase test reports
live in [`docs/design/`](docs/design/) and
[`docs/test_report/`](docs/test_report/). Tests: `go test ./... -race`.
CI builds linux/darwin/windows/android × amd64/arm64; releases carry
sha256 checksums and the install script.
