# Syl

A personal async voice AI assistant. Go backend, SQLite persistence, local network only.

Syl runs on your Linux box, serves a small browser UI over your LAN, and streams Claude
responses back to any connected tab via SSE. It remembers who you are (soul), can
schedule responses for later, and learns new capabilities by writing its own skill files.

---

## Quick Start

### Prerequisites

- Go 1.24+
- An Anthropic API key

### Run

```bash
export SYL_ANTHROPIC_API_KEY=sk-ant-...
make run
```

Open `http://localhost:8080` in a browser.

### Build

```bash
make build
./bin/syl
```

---

## Configuration

All config via environment variables, prefix `SYL_`:

| Variable                | Default       | Description                              |
|-------------------------|---------------|------------------------------------------|
| `SYL_ANTHROPIC_API_KEY` | (required)    | Anthropic API key                        |
| `SYL_PORT`              | `8080`        | Public port (UI + API)                   |
| `SYL_INTERNAL_PORT`     | `9090`        | Internal port (metrics, healthcheck)     |
| `SYL_DB_PATH`           | `data/syl.db` | SQLite database path                     |
| `SYL_NAME`              | `Syl`         | Assistant name                           |
| `SYL_SKILLS_DIR`        | `skills`      | Directory of skill Markdown files        |
| `SYL_DEBUG`             | `false`       | Enable debug logging + dev skills        |
| `SYL_REQUEST_TIMEOUT`   | `30s`         | HTTP handler timeout (not applied to SSE)|
| `SYL_SHUTDOWN_TIMEOUT`  | `30s`         | Graceful shutdown wait                   |

A `config.yaml` (gitignored) can be used locally instead of exporting env vars — see
`server/config.go` for field names.

---

## Development

```bash
make test      # run unit tests
make build     # build binary to bin/syl
```

Lint runs in CI only via `golangci-lint` (v2.10.1). To run locally:

```bash
golangci-lint run ./...
```

### Git workflow

All changes go through PRs. Never push directly to `main`.

```bash
git checkout -b feature/<short-description>
# ... make changes, bump VERSION ...
git push -u origin feature/<short-description>
gh pr create
```

CI gates: VERSION changed → lint → test → build (all must pass before merge).

---

## Project Structure

```
cmd/syl/          entry point
server/           HTTP server, router, handlers, config
internal/
  agent/          AgentStore — name-first identity, fingerprint fallback
  chat/           ChatStore — message log
  sse/            SSEBroker — fan-out to connected tabs, pending_events fallback
  jobs/           JobStore + Runner — scheduled tasks, survives restart
  classifier/     ClaudeClassifier — pre-call routing and soul updates
  claude/         Thin Anthropic SDK wrapper, streaming
  skills/         FSSkillLoader — reads skills/ at startup
  soul/           SoulStore — evolving user context on the agents row
  inbox/          InboxStore — open questions Claude surfaces
  db/             OpenDB() + migration runner
logger/           Request-scoped slog middleware
web/              index.html — browser UI (vanilla JS, SSE client)
skills/           Markdown skill files loaded at startup
skills/dev/       Dev-only skills (loaded when SYL_DEBUG=true)
docs/design/      Architecture and design documents
.claude/          Gitignored scratch space (spec, work tracking, scripts)
```

---

## Architecture

See `docs/design/agent-interactions.md` for the full design: agent identity, soul
evolution, message pipeline, SSE broker, inbox, skills, and job runner.

**Message flow (brief):**
1. `POST /message` resolves agent (name → fingerprint → create), persists message, returns 202
2. Goroutine: pre-classifier (small Claude call) → soul update → enqueue jobs
3. If immediate: assemble context → stream Claude → SSE tokens to browser
4. Browser tab receives tokens via `GET /sse?agent_id=<id>` (long-lived connection)

---

## Local Network Access (TODO)

The goal is for any device on the LAN (phone, laptop, tablet) to reach Syl at a
memorable hostname like `syl.local` or `syl.ai` without touching `/etc/hosts` on
each device.

**Planned approach — dnsmasq on the Linux host:**

```bash
# Install
sudo apt install dnsmasq

# /etc/dnsmasq.conf
address=/syl.ai/<LAN_IP_OF_HOST>   # e.g. 192.168.1.42

# Restart
sudo systemctl restart dnsmasq
```

Then point your router's LAN DNS server to the Linux host IP. All WiFi devices
will resolve `syl.ai` automatically.

**Fallback:** Add `/etc/hosts` entries per device manually:
```
192.168.1.42  syl.ai
```

**TODO items before this works:**
- [ ] Confirm LAN IP of Linux host (`ip addr`)
- [ ] Install and configure dnsmasq
- [ ] Configure router to use host as DNS server (or use fallback `/etc/hosts`)
- [ ] Verify `http://syl.ai:8080` resolves on a second device
- [ ] Consider binding Syl to `0.0.0.0` explicitly and confirming firewall allows port 8080

---

## VM / Network Exposure (TODO)

Syl runs on a Linux VM. This creates a network bridging problem: the VM has its own
IP that is not directly reachable by other LAN devices unless the VM's network adapter
is in **bridged mode** (not NAT).

**Checklist:**
- [ ] Check current VM network mode (NAT vs bridged)
- [ ] Switch VM NIC to bridged mode so it gets its own LAN IP
- [ ] Confirm VM IP is reachable from another device (`ping <vm-ip>` from phone)
- [ ] If bridged mode is unavailable: set up port forwarding on the Windows host
  (forward Windows host port 8080 → VM port 8080)
- [ ] dnsmasq should then run on the VM (bridged) or Windows host (with port forward)

**Windows host fallback:**

If VM networking cannot be made to work, Syl can be moved to the Windows host directly:

1. Volume-mount the `~/code/syl` directory into a WSL2 or native Windows Go environment
2. Build and run natively on Windows — modernc.org/sqlite is pure Go, no CGO required
3. dnsmasq is not available on Windows; use a hosts file or a lightweight DNS server
   like [Simple DNS Plus](https://simpledns.plus/) or Acrylic DNS

---

## Pending Work

### In progress / next up

| Issue  | Epic  | Title                                         |
|--------|-------|-----------------------------------------------|
| sy-09  | sy-E2 | Persistence integration test (real SQLite)    |

### Open epics

| Epic   | Title                  | What it covers                                              |
|--------|------------------------|-------------------------------------------------------------|
| sy-E4  | Pre-classifier + jobs  | Real ClaudeClassifier; JobRunner wired end-to-end; scheduled responses; soul mutation |
| sy-E5  | Skills + inbox         | FSSkillLoader wired; Claude writes skills; inbox voice command |
| sy-E6  | Polish                 | Message compaction; local DNS; TTS/STT; Prometheus metrics; integration test suite |

### Known gaps / tech debt

- [ ] `ClaudeClassifier` is not implemented — classifier package has only a fake
- [ ] `JobRunner` is not wired into the server — jobs are enqueued but never executed
- [ ] No web UI beyond a static skeleton — no real input form, no SSE display
- [ ] No message compaction — chat history grows unbounded
- [ ] No Prometheus metrics wired up despite the dependency being present
- [ ] `sy-09`: SQLite integration test missing (agent + chat round-trip against real DB)
- [ ] TTS/STT not started — browser Web Speech API planned; requires Chrome + internet
- [ ] No graceful handling when `SYL_ANTHROPIC_API_KEY` is missing at startup
- [ ] Local DNS / VM networking not yet set up (see sections above)
