# Agent Interaction Design

This document describes the intended design for how agents in Syl interact with
users, with the system, and potentially with each other over time.

---

## What Is an Agent?

An agent is a persistent identity representing a user (or device) interacting
with Syl. It is not the AI model — it is the _context carrier_: the named
persona, accumulated soul, and message history that shapes every Claude call.

Each agent has:
- **Identity**: a `name` (optional, human-assigned) and a `fingerprint`
  (browser-generated UUID, per-device).
- **Soul**: a short free-form text field that evolves over time, capturing
  things Syl has learned about this user — preferences, context, tone.
- **Message history**: the rolling conversation log, compacted periodically
  into summaries to keep context lean.

---

## Agent Resolution

Every inbound message resolves an agent before anything else happens:

```
name in request?  →  look up by name  →  found: use it
                                       →  not found: fall through
fingerprint present?  →  look up by fingerprint  →  found: use it
                                                  →  not found: create new agent
```

This means:
- A **named agent** (e.g., `agent_name=syl`) is shared across all devices
  that know the name — the intended primary path for a personal assistant.
- An **anonymous agent** identified only by fingerprint is per-browser,
  per-device. Useful for quick sessions without setup.
- Two devices using the same name converge to the same context, enabling
  continuity across phone, laptop, etc.

---

## The Soul

The soul is a compact, evolving description of the user as understood by Syl.
It is included in every Claude system prompt, giving all responses a grounded,
personalised feel without requiring the full history to be replayed.

**Soul updates** happen via the pre-classifier: on every message, a small fast
Claude call may return a `soul_update` string. If present, it overwrites the
current soul. The classifier is prompted to be conservative — only update when
something meaningfully new has been learned.

Example soul progression:
```
# Initial (empty)

# After a few conversations
Prefers concise answers. Works in Go. Interested in distributed systems.
Tends to ask about architecture before implementation.

# Later
Prefers concise answers. Senior Go engineer at a startup. Interested in
distributed systems and AI tooling. Asks architecture questions first,
implementation second. Sarcastic sense of humour. Coffee-dependent morning person.
```

The soul is readable and editable via `GET/PUT /agents/{id}/soul`, making it
transparent and correctable.

---

## Message Pipeline

```
POST /message
    │
    ├─ resolve agent (name → fp → create)
    ├─ persist user message
    │
    ├─ pre-classify (small Claude call, fast)
    │       └─ returns: soul_update | response_type | jobs | skill_names
    │
    ├─ apply soul_update (if present)
    ├─ enqueue jobs (if scheduled/multi)
    │
    └─ response_type?
            ├─ "immediate"  →  assemble context → stream Claude → SSE
            ├─ "scheduled"  →  jobs enqueued, 202 returned
            ├─ "inbox_read" →  fetch inbox items → format → SSE
            └─ "multi"      →  enqueue multiple jobs for compound response
```

The HTTP response is returned as `202 Accepted` immediately. All Claude work
happens asynchronously, delivered to the browser via the SSE stream.

---

## SSE and the Broker

Each agent maintains a long-lived `GET /sse?agent_id=<id>` connection per tab.
The SSE broker maps `agentID → []chan`. When Claude streams tokens, each token
is fanned out to all active channels for that agent simultaneously.

**Pending events fallback**: if no tab is connected when an event is published
(e.g., the user closed the browser between sending a message and the scheduled
job firing), events are written to the `pending_events` table. On reconnect,
the broker drains the table before resuming live stream.

This design means Syl's responses survive tab refreshes, brief disconnects, and
even overnight scheduled responses — the reply is waiting in the inbox when the
user returns.

---

## The Inbox

The inbox is a global table of open questions. It exists because there are
things Syl genuinely needs to know to be useful that it cannot infer from
conversation — the user's timezone, their name, project context, recurring
preferences.

Claude can write inbox items via `inbox_write` jobs. The user answers them via
the browser UI or a voice command ("what's in my inbox?"). Answered items are
retained in the table (status=answered) so they inform future context
assemblies.

Key design choices:
- **Global, not per-agent**: a single shared inbox. Any agent can read or write
  it. This reflects the single-user nature of Syl — it's a personal assistant,
  not a multi-tenant system.
- **Voice-accessible**: the pre-classifier recognises inbox inspection intent
  and returns `response_type: "inbox_read"`, which fetches and streams the
  open items as a formatted response.

---

## Skills

Skills are Markdown files in `skills/`. They represent callable knowledge —
how to handle specific domains, formats, or recurring task types.

At startup, all `.md` files in `skills/` are loaded. When `SYL_DEBUG=true`,
files in `skills/dev/` are also loaded. Skills are referenced by name in the
classifier output (`relevant_skill_names`) and injected into the system prompt
for the Claude call that follows.

Skills can be written by Claude itself, via `skill_write` jobs. This is the
mechanism by which Syl learns new capabilities through use — if it handles a
task well, it can codify that approach into a skill file for reuse.

Dev skills (e.g., `skills/dev/inbox_analysis.md`) are tools for the developer,
not the end user. They can introspect the system, propose plans, or analyse
inbox items during local development.

---

## Jobs and the Runner

The job runner enables Syl to be asynchronous and proactive:

- **Scheduled responses**: "remind me about X tomorrow morning" enqueues a
  `send_message` job with `run_at` set to the target time. The runner fires it,
  generates a Claude response, and delivers it via SSE.
- **Soul evolution**: `soul_update` jobs can be enqueued to refine the soul
  based on new information, outside of the immediate request cycle.
- **Inbox writing**: Claude can identify gaps in its knowledge and write
  `inbox_write` jobs to surface questions without blocking the current response.
- **Skill writing**: `skill_write` jobs allow Claude to persist a new skill
  file to disk when it synthesises a reusable capability.

Jobs survive server restarts (persisted in SQLite). Stuck jobs (running > 5
minutes) are automatically reset to pending by the runner's dead-job recovery
loop.

---

## Multi-Agent Scenarios (Future)

The current design is intentionally single-user. Future directions:

- **Agent handoff**: a named agent could receive messages from multiple devices,
  enabling seamless context continuity. Already supported by name-first
  resolution.
- **Shared inbox between agents**: the global inbox is already positioned for
  this — multiple agents asking questions into the same queue.
- **Agent-to-agent messaging**: a future job type (`agent_message`) could allow
  one agent to send a message to another, enabling automated workflows between
  personas (e.g., a "work" agent and a "personal" agent with different souls).
- **Compaction as long-term memory**: periodic compaction of message history
  into summaries means the soul and summary together form a durable, compressed
  memory that survives across many conversations.

---

## Context Assembly Order

When assembling the system prompt and message history for a Claude call:

```
1. Soul (agent's accumulated personality/context)
2. Relevant skill contents (injected by classifier)
3. Answered inbox items (not yet implemented — future)
4. Compaction summary (if exists)
5. Recent messages (up to threshold, since last summary)
6. Current user message (already in message list)
```

This ordering ensures the most stable, high-signal context comes first, with
the most recent interaction last — matching Claude's recency bias.
