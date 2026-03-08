# copilot-autocode — Copilot Orchestrator TUI

A sophisticated, local Go Terminal UI (TUI) application that acts as a
headless **Copilot Orchestrator**.  It manages a large queue of GitHub issues,
feeds them sequentially to the native GitHub Copilot coding agent up to a
configurable concurrency limit, and babysits the resulting Pull Requests
through CI feedback and merging.

---

## Features

- **Three-column Bubble Tea dashboard** — Queue / Active (Coding) / In Review
- **State-machine poller** ticking every 45 s (configurable) using only GitHub
  labels, assignees, PR states, and workflow-run statuses as state storage
- **Automatic Queue → Coding promotion** honouring `max_concurrent_issues`
- **Draft PR detection** — waits for Copilot to finish the initial coding pass
  before moving to review
- **Merge-conflict handling** — posts `@copilot Please merge from main…` and
  waits for the agent to finish before re-evaluating
- **3-Strikes CI AutoFix** — waits for a failure to appear on three consecutive
  poll ticks before posting the failure logs to Copilot for a fix
- **Auto-approve & squash-merge** when all CI checks are green and the branch
  is up-to-date
- **Graceful Ctrl-C shutdown** without corrupting any state

---

## Prerequisites

| Requirement | Version |
|-------------|---------|
| Go          | ≥ 1.22  |
| GitHub PAT  | `repo` + `workflow` scopes |

The PAT must have permission to:
- Read and write issues (labels, assignees, comments)
- Read and write pull requests (reviews, merges)
- Read Actions workflow runs and logs

---

## Installation

```bash
git clone https://github.com/BlackbirdWorks/copilot-autocode.git
cd copilot-autocode
go build -o copilot-autocode .
```

---

## Configuration

1. Copy the example config:
   ```bash
   cp config.yaml.example config.yaml
   ```

2. Edit `config.yaml`:
   ```yaml
   github_owner: "my-org"
   github_repo:  "my-repo"
   max_concurrent_issues: 3
   poll_interval_seconds: 45
   ```

---

## Usage

```bash
export GITHUB_TOKEN="ghp_…"
./copilot-autocode --config config.yaml
```

Press **q** or **Ctrl-C** to quit gracefully.

---

## Workflow Labels

The orchestrator uses three GitHub labels to track issue state.  They are
created automatically the first time the app runs if they don't already exist.

| Label       | Colour  | Meaning                                           |
|-------------|---------|---------------------------------------------------|
| `ai-queue`  | blue    | Issue is waiting to be handed to Copilot          |
| `ai-coding` | yellow  | Copilot is currently writing code for this issue  |
| `ai-review` | orange  | PR open; waiting for CI / merge                   |

To enqueue an issue, simply add the `ai-queue` label to it.

---

## Architecture

```
main.go
 ├── config/config.go       – YAML config loader
 ├── ghclient/client.go     – go-github wrapper (all GitHub API calls)
 ├── poller/poller.go       – state machine (runs as background goroutine)
 └── tui/
     ├── model.go           – Bubble Tea model & Update/View
     └── style.go           – lipgloss styles
```

### State Machine

```
         ┌──────────┐
         │ ai-queue │  ← label added manually by human
         └────┬─────┘
              │  promoteFromQueue (slots available)
              ▼
         ┌───────────┐
         │ ai-coding │  + assign copilot user
         └────┬──────┘
              │  PR no longer draft && no active agent run
              ▼
         ┌───────────┐
         │ ai-review │
         └────┬──────┘
         ┌────┴──────────────────────────────┐
         │                                   │
    branch behind?                    CI status?
         │                                   │
   post @copilot            failure (×3) → post fix request
   merge comment            success → approve + merge + close
```

---

## License

[MIT](LICENSE)
