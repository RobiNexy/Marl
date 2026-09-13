# Marl

**Multi-agent task runner in a single binary.** Give it a task; it forks
sub-agents to work in parallel, keeps every agent inside a sandbox, shows
you every agent in a live tree, and stops to ask you when something needs
a human decision.

[中文文档](README.zh-CN.md)

## When to use it

- You have a coding/writing task big enough to split (e.g. "implement a
  solver + its UI + tests") and want the pieces done **in parallel** by
  separate agents that don't step on each other.
- You want to **stay in the loop**: agents check in with you via files you
  can read and edit in any editor — no extra UI required.
- You want **cost visibility**: every LLM call is recorded and priced.

## 30-second quick start

Requirements: [fossil](https://fossil-scm.org) 2.20+ on PATH, and a
DeepSeek API key.

```bash
marl init ~/my-task                      # one-time: create a project
cd ~/my-task
export DEEPSEEK_API_KEY=sk-...

marl start "Read README.md and summarize it in one sentence"
# → runs a project agent, prints progress, and drops the final report
#   into your inbox: ~/.local/state/marl/my-task/inbox/report_*.md
```

While it runs (in another terminal):

```bash
marl status                              # live tree: who is running, who is blocked
marl say -to sub_000001 "Prefer Chinese for the summary"   # message a running agent
marl log -db .marl/store.db              # full conversation
```

When it finishes, the agent's report is a file in your inbox. Read it with
any editor.

## Install

```bash
# From source (Go 1.27+):
git clone https://github.com/RobiNexy/Marl && cd Marl
go install ./cmd/marl

# After the first tagged release:
go install github.com/RobiNexy/Marl/cmd/marl@latest
```

Check your setup:

```bash
marl doctor    # fossil present? API key set? project skeleton healthy?
marl version
```

## The three ideas behind Marl

**1. You are the root of the tree.**
Every project has one human (you). Agents you spawn form a tree below you;
any agent can hand work further down. If something goes wrong at the
bottom, the failure walks back up until someone can act on it — that's you
at the top.

**2. Tasks fork into parallel sub-tasks.**
An agent that splits its work forks child agents with `spawn_batch`; each
child can fork its own. Every child gets an explicit write-scope (a set of
path globs) — a child physically cannot write outside it. When a child
finishes (or crashes), its parent receives a report; the framework reports
on a child's behalf if the child dies silently.

**3. It stops and asks you.**
Expensive or risky operations pause the agent and drop a request file into
your inbox. Edit the file to answer (e.g. replace the `@grant once` line
with `@grant next 20` to approve the next 20 of something), and the agent
resumes. Discussions work the same way: the agent proposes, you annotate
or approve, the outcome is checked into version control under your name.

Everything is observable: full conversation logs (append-only), an audit
trail, and a cost report. Agents can't fake approvals — approval files
live outside every agent's write scope.

## CLI reference

| Command | What it does |
| :-- | :-- |
| `marl init [dir]` | Create a project (fossil repo + `.marl/` skeleton) |
| `marl start "task"` | Run a task (attached; Ctrl-C to interrupt) |
| `marl start --detach "task"` | Run in the background (log in the control plane) |
| `marl say -to <agent> "text"` | Message a running agent |
| `marl status` | Live supervision tree + blocked/waiting states |
| `marl stop [-force]` | Gracefully stop the background task |
| `marl log -db <db> [-out file.md]` | Export / print conversations |
| `marl attach -db <db>` | Tail new entries (2s poll) |
| `marl knowledge lint` | Check the always-on knowledge block budget |
| `marl knowledge promote/pull` | Share knowledge across projects (fossil-based) |
| `marl models probe -models m1,m2` | Check a model: chat / tool-call / cache |
| `marl doctor` | Environment self-check |
| `marl version` | Build info |

Your inbox lives at `~/.local/state/marl/<project>/inbox/`:

| File | Meaning | Your action |
| :-- | :-- | :-- |
| `report_*.md` | An agent's final report | read it |
| `gate_*.md` | Approval request | edit the `@grant …` line, save |
| `direct_*.md` | Message you sent (`marl say`) | — (auto-archived) |

`@grant once` / `@grant next 20` / `@grant tokens 50000` / `@always-grant`
(permanent, saved to `grants/`) / `@deny`.

## Configuration

All configuration is per-project YAML under `.marl/` (version-controlled
with your project):

| File | What it controls |
| :-- | :-- |
| `.marl/config.yaml` | Budget limits (per-task call counts, token caps, timeouts), approval rules, sidecar wires |
| `.marl/profiles/*.yaml` | Per-agent role: which model capabilities it needs, sampling params (temperature, max tokens, timeout), skill whitelist, whether it may fork |
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

Run `marl doctor` after editing to catch mistakes early. The YAML parser
is intentionally strict: unknown keys and tab indentation are errors, not
silently-ignored typos.

## FAQ / Troubleshooting

**Where does my inbox live?** `~/.local/state/marl/<project>/inbox/`
(override the root with `XDG_STATE_HOME`).

**How much did a run cost?** `go run ./cmd/ladder_report -db .marl/store.db
-task start-task` from the Marl repo, or query `.marl/store.db`
(`ledger_entries`) directly.

**A sub-task failed. Is everything lost?** No. Failures are reported up
the tree (the framework reports on behalf of crashed agents), the root
agent adapts or retries, and the full history stays in the log.

**An agent is stuck "blocked".** It's waiting on a human file in your
inbox (or a discussion verdict). Nothing is being spent while blocked; the
watchdog raises an alert if the wait exceeds 24h.

**The agent keeps writing files I didn't ask for / ignores my mid-run
message.** Messages via `marl say` are injected between rounds — they
don't interrupt work already in progress.

**Keys and secrets:** export `DEEPSEEK_API_KEY`; never commit it.

## For contributors

Design documents, architecture decisions (ADRs), and phase test reports
live in [`docs/design/`](docs/design/) and
[`docs/test_report/`](docs/test_report/). Tests: `go test ./... -race`.
