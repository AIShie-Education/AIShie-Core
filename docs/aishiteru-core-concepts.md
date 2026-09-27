# AIshiteru — Core Concepts

*An agent-centered LMS where AI agents are primary actors, not bolted-on features.*

> Concrete data model: [schema.md](./schema.md). This document states intent; schema.md states what is built.

---

## 1. Primitive: Event

Every meaningful thing that happens in the system — a grade submitted, a message sent, a quiz generated, a student flagged — is modeled as an **event**. This is the domain-level unit of meaning: it's how the system names and reasons about "what happened."

This is *not* full event-sourcing. Events aren't the sole source of truth requiring replay to reconstruct state.

## 2. State: Mutable, Relational

An event triggers a state update; state itself is ordinary mutable relational data reflecting the current truth. Events can still be logged for audit/debugging, but the read path never needs to replay history — it just queries current state.

**Why this pairing:** Event gives you a clean vocabulary for domain logic and side effects; relational state keeps queries simple and avoids event-sourcing's replay complexity.

## 3. Actors: Unified Human + Agent

Humans and agents are **the same kind of actor**, exposed through one surface — both the UI and MCP terminate in the same underlying tool calls. There is no separate "human API" and "agent API."

Differentiation happens entirely through **course membership**, not actor-type branching in code. A human and an agent each join a course as a member with a role, a set of per-action permissions and a scope; a tutor agent bound to one student and a human tutor for the same student hold the same membership shape. Access control is the *only* place identity matters, and it never reads whether the member is human or agent.

**Consequence:** "UI is just a client of the tool layer" falls out for free — it doesn't need to be separately engineered.

**Agents people own:** a person may register agents of their own and bring them into a course where they are seated. Such an agent acts only as its owner's *delegate*: its seat's principal is the owner's seat, and it can never do, reach or outlast more than that seat. Owning an agent is a way to do what one may already do, by a program running elsewhere — never a way to more. It is still membership that decides, not actor type: the cap is the principal's seat, read on every call.

**Where agents live:** Agents are *registered* here — actor record, credentials, course memberships — but they run entirely outside. Core never dials out to an agent; agents connect *in* and make tool calls, exactly as a browser session does for a human. No endpoint, model, or prompt is stored anywhere. Two consequences worth stating plainly: core can only govern what crosses its own boundary, and work discovery is pull-based — agents come looking for work rather than being dispatched to it. [agent-runtime.md](agent-runtime.md) is the handout for a service that runs them.

## 4. Autonomy: Explicit per Action-Type

Autonomy is not inherited from actor type and not set by a single global policy. Every **action type** (submit grade, send message, edit material, flag student, etc.) carries its own explicitly declared autonomy level:

- **Autonomous** — executes, no human in the loop
- **Pending-review** — executes immediately, then enters a human review queue
- **Confirm-required** — blocked *before* execution; becomes a proposal awaiting a human
- **Denied** — not permitted at all

These form one ordered ladder, `denied < confirm_required < pending_review < autonomous`, which is also the return type of the permission check. Permission and autonomy are not two systems: the check answers *may this actor act* and *how* in a single value. Keeping `denied` inside the ladder is what lets a narrow exception — "grade this course, but not this one student" — be expressed through the ordinary mechanism instead of a separate deny-rule engine.

Note that `pending_review` and `confirm_required` both involve a human, at opposite ends: one reviews after the fact, the other gates beforehand.

**Presets** (student, tutor, grader, ...) pre-populate common action-type × autonomy-level combinations. They live in a `permission_preset` table — eight built-ins seeded at install, plus any a department defines for itself — and are copied onto the membership row when a member is added; the row is the only thing the permission check reads, so a preset is a convenience, not a source of truth, and any single value can be overridden afterwards.

**Scope** narrows a membership further: to listed students, to listed assignments, or both. Scope is explicit and fails closed — a member limited to a list with nothing on it can touch nobody.

## 5. Memory: Outside the Core

Memory is an agent concern, not a platform concern. Agents live elsewhere, so they keep their memory elsewhere. Core stores none of it.

What core owes them instead is two things:

- **A stable handle.** A course membership *is* the relationship — "tutor for Yuki in CS101" is one `course_member` row with a permanent id — so an external agent can key its own store against `course_member.id` and have a durable notion of which relationship it is resuming. Removing and re-adding an agent gives it a new id, and therefore a fresh start.
- **A read surface good enough to cold-start on.** Since core holds no memory, *every* agent rebuilds its picture of a student or a course on every invocation. That makes the read tools load-bearing in a way they would not be if the platform cached context on the agent's behalf.

The original relationship-bound vs. task-scoped distinction survives as a description of how agents *behave*, not as something the platform implements. A tutor agent accumulates context across a semester because its host chooses to; core cannot tell the difference and does not need to.

## 6. Termination: Explicit per Job-Nature

Like autonomy, termination criteria are declared per agent-role/task-type rather than following one global rule:

- **Fixed condition** — task is objectively complete (e.g. batch fully graded)
- **Budget** — cap on tool calls or wall-clock time
- **Human interrupt** — a person ends it
- **Self-assessed stopping** — the agent judges it's done

**Where this is declared:** on the course membership, which *is* the agent-role — an actor, a course, a set of permitted actions and a scope. v1 ships the human-interrupt and fixed-time forms only: pause, remove, and `expires_at`. Call and wall-clock budgets, with their per-run counters, are deferred until an agent workload needs them.

**What is actually enforceable:** tool calls and wall-clock time. Core performs no inference and never sees a model call, so a token cap can only be self-reported, never enforced. Budgets that sound equivalent are not.

This connects directly to the ICoT research question of what happens when agent loops can't or don't decide to stop — an undefined termination criterion is the same failure mode in a different costume.

## 7. Content: Document-Primary, Graph as Hidden Skeleton

Course material is authored and stored as **ordinary documents** — no graph-authoring burden placed on profs. An agent **derives a concept graph** in the background (concepts, prerequisites, relationships) as a **projection of the documents**, not an independent source of truth.

- The graph is **optionally viewable** but not the primary interface — most interaction stays document/conversation-based.
- **Document-facing agents** (tutor answering a direct question, TA grading against a rubric) query documents directly.
- **Structural agents** (curriculum agent, tutor reasoning about "what's next") query the graph.
- Since the graph is auto-generated rather than hand-authored, there's no meaningful cost to it always existing — no course needs to "opt in" to structure.

**Status: deferred.** The graph is an add-on. v1 stores and serves documents; nothing derives from them yet. Everything above describes the intended shape for when it lands.

**Open sub-question (deferred):** staleness/re-derivation triggers — when documents change, does the graph regenerate automatically, on-demand, or drift until requested?

---

## Summary Table

| Concept | Decision |
|---|---|
| Primitive | Event (domain-level meaning) |
| State | Mutable/relational, updated by events |
| Actors | Unified human + agent, one surface, differentiated only by course membership; registered here, hosted elsewhere |
| Autonomy | Explicit per action-type; one ordered ladder shared with permission; presets as table rows, scope fails closed |
| Memory | Not in core — agents keep their own; `course_member.id` is the stable relationship handle |
| Termination | Declared on the membership; v1 = pause, remove, expiry; budgets deferred; tokens never enforceable |
| Content | Document-primary; agent-derived concept graph deferred as an add-on |

---

## Tech Stack

AIshiteru is a **pure orchestration environment** — no model hosting, no inference. All AI is separately hosted and plugged in through MCP. The system itself is state, events, and a tool surface (MCP + UI/API) that external agents connect to. This makes it an I/O-bound, concurrency-heavy system (many simultaneous sessions mostly waiting on external agent calls) rather than a compute-bound one — which drove the language choice.

| Layer | Decision | Why |
|---|---|---|
| **Backend language** | Go | Fits I/O-bound, concurrency-heavy workload directly via goroutines/channels; officially maintained MCP SDK; single static binary, easy horizontal scaling; low ceremony for fast iteration while design is still evolving |
| **MCP layer** | Official Go MCP SDK | Literal transport for all agent tool-calls — the mechanism by which the unified actor model is exposed to agents |
| **Non-MCP API** | REST (HTTP + JSON) | Keeps UI, API, and MCP all speaking the same wire format (MCP is already JSON-RPC-based); avoids introducing a third format the way gRPC would; strong typing already comes from Go itself, so gRPC's compile-time contract benefit is less needed |
| **Database** | PostgreSQL | Holds mutable relational state (per the State concept) and the event table; will also host the agent-derived concept graph, either via adjacency tables/recursive queries or a graph extension (open decision) |
| **Frontend** | Separate app (e.g. React) | Go does not double as a templating/UI layer; frontend consumes the same REST API |
| **Auth / permissions** | Custom, not off-the-shelf RBAC | Per-action-type autonomy levels on a course membership, plus student/assignment scope, don't map onto typical role-based auth libraries. Login itself is delegated: SSO (PolyU ADFS) for humans, API tokens for agents |
| **Cache** | None in v1 | Postgres serves reads directly; `event.seq` is already the cursor a Redis stream would use if push notifications are needed later |

**Rejected alternatives (for context):**
- **Rust** — strongest correctness guarantees, but more ceremony and slower iteration while schema/API shape is still in flux
- **Elixir/OTP** — actor-model fit is elegant (BEAM processes ↔ agents), but smaller MCP ecosystem and a bigger conceptual jump
- **gRPC** — better for streaming and service-to-service traffic, but would add a third wire format alongside REST and MCP's JSON-RPC; a narrow WebSocket/SSE channel can be added later if true server-push is needed (e.g. live-updating dashboards)

**Open sub-decision (deferred with the graph):** how the concept graph is represented in Postgres — recursive CTEs over adjacency tables vs. a graph extension (e.g. Apache AGE). Not a v1 question.
