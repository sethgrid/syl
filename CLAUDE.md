# Syl — Project Context

## Overview
Syl is a personal async voice AI assistant. Go backend, local network only, SQLite persistence.

## Stack
- Go 1.25, chi, slog, modernc.org/sqlite (pure Go), Anthropic Go SDK
- caarlos0/env, kverr, testify, errgroup, Prometheus

## Module
`github.com/sethgrid/syl`

## Key Packages
- `internal/agent` — AgentStore (name-first identity, fingerprint fallback)
- `internal/chat` — ChatStore + message log
- `internal/sse` — SSEBroker (subscribe/publish, pending_events fallback)
- `internal/jobs` — JobStore + Runner (run_at polling, survives restart)
- `internal/classifier` — ClaudeClassifier (small pre-call)
- `internal/claude` — thin Anthropic SDK wrapper, streaming
- `internal/skills` — FSSkillLoader (reads skills/ at startup)
- `internal/soul` — SoulStore (TEXT column on agents table)
- `internal/inbox` — InboxStore (open questions Claude writes)
- `internal/db` — OpenDB() + migration runner

## SQLite
Open with `?_journal_mode=WAL&_busy_timeout=5000`

## SSE
- Long-lived GET /sse per agent. No RequestTimeout on this route.
- Broker maps agentID → []chan
- On no active tab: write to pending_events
- On reconnect: drain pending_events first, then live stream

## Pre-classifier Flow
1. Resolve agent (name > fingerprint > create)
2. Persist user message
3. Small Claude call → JSON: {soul_update, response_type, jobs, relevant_skill_names}
4. Apply soul_update if present
5. Enqueue jobs if scheduled
6. If immediate: assemble full context → Claude stream → SSE tokens → persist response

## Environment Variables
Prefix: `SYL_`

## Port Conventions
- Public: 8080 (default)
- Internal (metrics/health): 9090 (default)

## Reference
`~/code/helloworld` — canonical patterns to mirror.
Spec at `.claude/spec.md` (gitignored).
