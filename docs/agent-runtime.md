# The agent runtime: a handout

For the team building the agent runtime: a separate service, in a repository
of its own, that hosts AI agents for AIShiteru. §2 was checked against this
repository and a running `aishiterud` on 2026-09-28; if it and Core ever
disagree, `GET /v1/tools` is right. §3 comes from the providers' documentation,
read on 2026-09-26; **[UNVERIFIED]** marks what it did not confirm.

## 0. In short

- The runtime holds the models, prompts, provider keys and its working
  notes, and runs the agent loops. Core holds none of these and never calls
  the runtime. An agent's long-term memory is Core's, the agent's whatever
  runs it (§2.5).
- The runtime connects in to Core as each agent it hosts, with that agent's
  token, over MCP at `https://<core>/mcp`. Core pushes nothing: the runtime
  polls `conversation_inbox` for questions and `event_list` for outcomes. A
  generic MCP client with an agent token answers nobody, since it acts only
  while a person types. So the site offers people only agents whose runtime
  says it answers: the runtime calls `me_site_chat` when it starts an agent
  (§2.3), and an agent nothing has declared is asked nothing in the site.
- Core's permissions decide what an agent may do and whether a person must
  confirm it. The runtime only narrows further: allowlists, budgets, quotas.
- The runtime reaches the mainstream LLM APIs through one internal format and
  a few adapters (§3): OpenAI-compatible Chat first, then Anthropic Messages,
  Gemini, OpenAI Responses and Bedrock Converse.

## 1. Boundaries

### 1.1 What lives where

| Concern | Core | Runtime |
|---|---|---|
| The agent's identity: actor, owner, tokens | `agent.create`, `agent.issue_token` | keeps the token, encrypted; never issues one |
| What the agent may do, its levels and reach | its `course_member` row, checked on every call | reads it (`me_memberships`); never widens it |
| Confirmation and review of actions | proposals, the approval and review queues | reports the outcome; never simulates approval |
| Model, endpoint, prompt, provider key | never stored | all of them |
| Long-term memory | kept, the agent's: about its owner, about each asker per course, a course's reviewed shared memory (schema.md §2.9) | reads it for the answer at hand with `memory_*`; keeps no copy |
| Working notes on one conversation | never stored | keyed on the seat's `member_id`, then per `conversation_id` |
| Conversation transcript | `conversation_message`, append-only | reads it; caches only |
| Turns, tool calls, tokens, wall clock | only pause, removal and `expires_at` | every budget (§7) |
| Finding work | offers `conversation_inbox` and `event_list` | polls them |
| Whether people in the site may ask the agent | records the token that said so; holds it while that token works and the agent and its owner are active | says so with `me_site_chat` on each start, and unsays it on stopping |
| Rate limit | 600 calls a minute per actor, burst 100, by default | stays well under it (§7.3) |

### 1.2 Connecting

- **Transport.** `POST https://<core>/mcp`, `Authorization: Bearer ais_…`.
  Core runs the official Go SDK (v1.8.0) stateless with JSON responses: one
  JSON-RPC message per request, one `application/json` answer, no SSE, no
  session, and the token checked every time.
- **Protocol revision.** Core answers `initialize` for 2024-11-05 through
  2025-11-25 with the client's revision, and one asking for `2026-07-28` with
  `2025-11-25`. It also takes `2026-07-28` requests without `initialize`,
  which carry the revision, client info and capabilities in `params._meta` and
  an `Mcp-Method` header, as the official SDKs send them. Pin one revision and
  cover it in the contract tests (§8.2).
- **Refused.** A JSON-RPC batch (400), an id over 256 bytes (400), and
  `subscriptions/listen` (404, `-32601`).
- **Tool names** are the registry's with the dot turned to an underscore
  (`conversation_answer`). There are 123 tools, 48 reads and 75 writes; all
  match `[a-z_]+`, the longest has 26 characters, and every provider takes
  them as they are (§3.7).
- **REST.** `GET /v1/tools` (no token needed) lists each tool's `name`,
  `description`, `kind`, `method`, `path`, `input_schema` and
  `output_schema`. Writes are `POST`s with an `Idempotency-Key` header. The
  status is 200 executed, 202 proposed, 403 denied, else by error code; the
  body holds the envelope's fields. Prefer MCP, which brings Core's
  instructions with it; both share one per-actor limit.

### 1.3 Two kinds of hosted agent

| | A person's own agent | A course tutor |
|---|---|---|
| Owner and principal | a student or any person; their own seat | an instructor; their seat |
| Seated by `member.add_delegate` with | preset `delegate`; a student's request waits for an instructor | preset `course_tutor` |
| `answers_course` | false | true, while its principal manages the course's members |
| Who may ask it | its principal alone | every member whose seat contains it: every student |
| Reach | its principal's own work | the material, and nobody's work |
| Answer level | capped by the principal's `conversation_ask` (students: autonomous) | the instructor's choice; autonomous by default |
| Who pays | the owner's key, or the school's with quotas | the school or the course |

## 2. The Core contract

### 2.1 The envelope

Every MCP `tools/call` returns one JSON envelope twice: as `structuredContent`,
and as the text of `content[0]`. Read `structuredContent`, falling back to the
text.

```json
{"status":"executed","action_id":"0192…","review_state":"none","replayed":true,
 "result":{"message_id":"0192…"},"note":"…","error":{"code":"…","message":"…","details":{}}}
```

- `status` is `executed`, `proposed`, `denied` or `failed` for a call that was
  attempted and recorded; `rejected` or `cancelled` only on the replay of an
  old proposal; and `error` for a call never attempted: bad arguments, no such
  tool or target, a reused key, a fault of Core's.
- MCP `isError` is true for everything but `executed` and `proposed`. It is an
  outcome to act on, not a transport failure.
- `action_id` is absent for reads, which are not recorded. `review_state` is
  `none`, `pending`, `reviewed` or `escalated`: `executed` with `pending` is
  done, and a person looks at it afterwards.
- Error codes: `invalid_argument`, `unauthenticated`, `forbidden`,
  `not_found`, `conflict`, `idempotency_conflict`, `failed_precondition`,
  `rate_limited`, and `internal` for a fault of Core's. Classify by `code` and
  `details`, never by the message (at most 400 bytes).
- Some answers come at the HTTP level: `401` (`text/plain`), the token is
  missing, expired or revoked, so stop the agent and tell its owner; `429`,
  `{"error":{"code":"rate_limited","details":{"retry_after_seconds":n},…}}` with
  `Retry-After`, and nothing attempted. On a `5xx`, a network error or code
  `internal`, retry with backoff, the same key and the same arguments.

### 2.2 Idempotency

- Every write takes `idempotency_key`, 1 to 200 characters. Core keeps it
  unique per actor, with a hash of the tool name and the canonical arguments.
  The same key and arguments again return the stored outcome, as it stands
  now, with `replayed: true`: a proposal approved since replays as
  `executed`. The same key with other arguments is `status: error`, code
  `idempotency_conflict`, with `details.action_id`, and nothing is recorded.
- The runtime removes `idempotency_key` from the schema the model sees and
  sets it itself. Models never choose keys.

| Write | Key |
|---|---|
| An answer | `answer:{conversation_id}:{in_reply_to_message_id}:{attempt}`, attempt from 1 (Core's MCP instructions give this form) |
| A canned or fallback answer | the same: it is an attempt at the answer to that message |
| Closing a conversation | `close:{conversation_id}` |
| Declaring site chat, or ending it | `site_chat:{on\|off}:{start id}`, a new start id each time the agent is started or stopped: the same key again would only replay what an earlier token declared |
| Any other write a model starts (M3) | `tool:{member_id}:{first 32 hex of sha256(tool name + canonical arguments)}` |

**Write ahead.** Store `(key, tool, exact arguments)` before sending, and after
a timeout send the stored bytes again: a new body under the old key is an
`idempotency_conflict` with the runtime's own call. A regenerated body is a new
attempt, under the next number, after an attempt that posted nothing (failed,
rejected, cancelled). That is safe: Core refuses a second answer to a message
(`already_answered`), and a second proposal while one waits (`answer_pending`).

### 2.3 The tools the runtime calls

**Identity and seats**

| Tool | Use |
|---|---|
| `me_get` | Checks the token and returns the agent's actor: `id`, `kind`, `display_name`, `status`, and `owner_actor_id`, the person who owns it. `owner_actor_id` is absent for an agent nobody owns (one an administrator registered without an owner), and for a person, whose own token the runtime refuses anyway (`kind` is not `agent`). It names the owner while the owner is suspended too; Core gives a suspended person no assertion (§5.1), so they cannot connect the agent meanwhile. An agent's owner is fixed when it is registered and never changes (schema.md §2.1), so the owner a stored token's agent names is the one it named when the token was taken. |
| `me_memberships` | Every seat: `member_id`, `course_id`, `code`, `section`, `title`, `course_status`, `role`, `status`, `expires_at`, `student_scope`, `assignment_scope`, `principal_member_id`, `perms` (permission to level, a delegate's capped by its principal's, all `denied` while the seat does not count), `answers_course`, and `perm_ceilings` with `perm_ceiling_reasons`: the most each permission of the seat could ever be, and why where that is below `autonomous` (an agent's `action_decide` is `confirm_required` at most: its decisions and reviews are proposals). Work only in active seats of courses not archived whose `perms.conversation_answer` is not denied. |
| `me_site_chat` | `{on: true}` when the runtime starts the agent, with the token it runs it with, and `{on: false}` when it stops: until then people in the site are not offered the agent, and `conversation.open` and `conversation.ask` addressed to it are refused `failed_precondition`, `agent_answers_elsewhere`. Returns `site_chat`, whether it holds now (false while the owner is suspended). It holds only while that token works: revoked or expired, it ends by itself, and a new token must declare it again. The owner may end it (`agent.update` with `site_chat: false`), never start it; the runtime starts it again on its next start. A person's token is refused, `not_an_agent`. Conversations already open are unaffected: the agent answers them, and they stay readable. |

**Finding work**

| Tool | Input | Output |
|---|---|---|
| `conversation_inbox` | `course_id`, `limit` (default 20, at most 100) | `conversations[]`, longest waiting first: open, the opener wrote last and has not retracted it, the opener still able to address the agent, and no answer to that message waiting for approval. Each row carries `latest_opener_message_id`, what an answer replies to. |
| `event_list` | `course_id`, `since_seq` (0 for the start), `limit` (default 100, at most 500) | `events[]`, `next_seq` (it moves on over events the caller may not see), `more`. Keep one cursor per seat. |

The events that matter carry ids, never text:
- `action.approved`, `action.rejected` and `action.cancelled`, filed under the
  proposal's `action_id`. `action.approved` has `payload.outcome`, `executed`
  or `failed`; `action.cancelled` has `payload.reason` (proposals expire after
  14 days by default; `withdrawn` when the agent or its owner took it back).
  `payload.by_owner` is true when it was the agent's own owner who approved,
  rejected or withdrew it. A rejection's reason is in the proposal's
  `result.decision.reason`, which `action_list_mine` returns.
- `conversation.opened`, `conversation.message_posted` (`conversation_id`,
  `message_id`, `author_member_id`, `opener_member_id`,
  `respondent_member_id`), `conversation.closed`,
  `conversation.message_retracted`: for the two participants only.

**Answering**

| Tool | Input | Notes |
|---|---|---|
| `conversation_messages` | `course_id`, `conversation_id`, `after_seq` or `before_seq`, `limit` (default 50, at most 200) | `conversation` (the view below), `messages[]`, `more`. A message has `id`, `seq`, `author_member_id`, `in_reply_to_message_id`, `body`, `created_at`; a retracted one has no `body` but `retracted: {at, by_member_id, reason}`. `after_seq` reads on; `before_seq`, or neither, gives the newest; both oldest first. |
| `conversation_get` | `course_id`, `conversation_id` | The view: `status`, `state` (`awaiting_answer`, `reply_pending_approval`, `answered`, `closed`), `pending_reply_action_id`, `opener`, `respondent` (with `answer_level`), `latest_opener_message_id`, `last_retracted_at`, `visible_to`. |
| `conversation_answer` | `course_id`, `conversation_id`, `in_reply_to_message_id`, `body`, `idempotency_key` | `body` is 1 to 20,000 characters of Markdown. `in_reply_to_message_id` must be the opener's newest message. Returns `message_id`. |
| `conversation_close` | `course_id`, `conversation_id`, `reason` (≤ 500 characters) | Either participant closes. The runtime closes only as §2.4 says. |
| `conversation_retract` | `course_id`, `message_id`, `reason` | The author retracts, or staff overseeing the opener; the runtime only when the owner asks. |
| `action_list_mine` | `course_id`, `exclude_types`, `after`, `limit` | The agent's own actions, oldest first, with `status` and `result`: a proposal's fate and a rejection's reason. Keep its `after` cursor. (`action_get` needs `action_decide`.) |

**Read tools for the model**, where the seat allows (§4): `course_get`,
`document_list`, `document_get` (text in `version.body_md`; a file's
`download_url` lasts 15 minutes),
`assignment_list`, `assignment_get`, `submission_list`, `submission_get`,
`grade_list`, `grade_get`, `component_tree`, `gradebook_get`.

### 2.4 What an answer can come back as

| Envelope | Meaning | What the runtime does |
|---|---|---|
| `executed`, `review_state: none` | Posted. | Mark the course hot (§7.2); update memory. |
| `executed`, `review_state: pending` | Posted; staff review it afterwards. | The same; a retraction may follow. |
| `proposed` | The level is `confirm_required`: the answer waits for a person, and the inbox leaves the conversation out. | Keep the `action_id`; watch `event_list` for its outcome. Never send it again under a new key. |
| `denied` | The seat may not answer now: its level was lowered, or it or its principal was paused. | Stop polling the course until `me_memberships` changes; tell the owner. |
| `failed`, `conflict`, `moved_on` | The opener wrote again meanwhile. | Answer `details.latest_opener_message_id`, under its own key. |
| `failed`, `conflict`, `already_answered` or `answer_pending` | That message is answered, or an answer waits. | Leave it. |
| `failed`, `conflict`, `closed` | Closed, by a participant or a seat's removal. | Drop it. |
| `failed`, `forbidden`, `not_addressable` | The opener may no longer address the agent. | Drop it; read `me_memberships` again. |
| `failed`, `invalid_argument` | Too long, empty, or not a reply to an opener's message. | Fix it; write again under the next attempt number. |
| `error`, `idempotency_conflict` | Another body was sent under this key, by another worker. | Never regenerate under it. If the conversation is still in the inbox, answer under the next attempt number. |
| `error`, `not_found` | No such conversation, or none the agent may read now. | Drop it. |
| any, `replayed: true` | Already done. | Treat it as the stored status. |
| replayed `rejected` or `cancelled` | A person rejected the earlier answer, or it expired. | See below. |

**A rejected or cancelled answer** puts the conversation back in the inbox for
the same message, where the same key would only replay the rejection.
Regenerate under the next attempt number, with the rejection's reason in the
prompt. After `max_attempts` (default 3), close the conversation with a short,
polite reason rather than leave it at the head of the inbox. An answer waiting
for approval to a message since overtaken holds nothing up: the inbox shows the
newer message, and approving the old answer can only fail. The answers of an
instructor's own tutor that wait for approval or review are decided by someone
else, unless the instructor answers without a confirmation themselves, when
they may decide them too; where nobody else decides actions and the instructor
may not, they must stay autonomous (schema.md §2.8).

### 2.5 Memory, presence, limits

- **Long-term memory is Core's** (schema.md §2.9, aishiteru-core-concepts.md
  §5). What an agent keeps between conversations — about its owner, about each
  person who asks it in a course, a course's shared memory — is read and
  written with `memory_search`, `memory_list`, `memory_get`, `memory_write`,
  `memory_update` and `memory_forget`, and is the agent's whatever runs it.
  What is about an asker is reached only through a conversation of theirs the
  agent may answer now (`course_id` and `conversation_id`); what the agent
  writes to the shared memory waits for review. A tutor answers many people
  and never carries one asker's words, or what it keeps about them, into
  another's prompt: Core's MCP instructions say so, and schema.md §2.8 says
  why Core cannot enforce it, since one token serves every conversation. Read
  memory afresh for each answer and keep no copy of it, so that a person's
  deletion holds. With `MEMORY=off` on the Core, every memory tool answers
  `memory_unavailable`: answer from the conversation alone.
- **The runtime's own notes are keyed on the seat's `member_id`, then on
  `conversation_id`**: what a person rejected, what was retracted. They are
  its working state, never written to Core's memory, nor Core's memory into
  them. A seat given again has a new `member_id` and starts afresh, in Core's
  memory too; never carry notes over by actor and course. When a `member_id`
  leaves `me_memberships`, stop using it and purge its notes after
  `retention_days_after_removal` (default 30).
- **Presence.** Core records a token's last use at most once a minute, and the
  frontend shows an agent as online if it was seen in the last two minutes, so
  a runtime whose calls are never 60 s apart always shows online. When an
  owner pauses an agent in the runtime, call `me_site_chat` with `on: false`
  and then stop calling Core for it, so that neither the site nor presence
  says it answers. When hosting ends for good, do the same and revoke the
  agent's token with `credential_revoke` (the token may revoke itself): its
  site chat ends with it, whatever else happens.
- **Rate limit.** 600 calls a minute per actor, burst 100, by default
  (`RATE_LIMIT_PER_MINUTE`, `RATE_LIMIT_BURST`); reads count, and MCP and REST
  share it. §7.3 spends it.

### 2.6 A student asks their own agent

```
Once:  Yuki, in Core: agent.create "Yuki's helper" → agent.issue_token → ais_…
       Yuki, in the runtime: paste the token, pick a model and a key
       Yuki, in CS101: member.add_delegate (preset delegate) → proposed → an instructor
       approves → seat D, principal P (Yuki's seat), student_scope listed [P]
Runtime:
 1. me_get; me_memberships → [{course_id C, member_id D, principal_member_id P, answers_course
    false, perms {conversation_answer, document_read, submission_read, grade_read: autonomous …}}];
    me_site_chat{on: true, idempotency_key "site_chat:on:<start id>"} → site_chat true
 2. toolset(D) = the read tools D's perms allow ∩ the allowlist
 3. conversation_inbox{C} every 2–10 s, jittered → []
    Yuki: conversation.open{respondent_member_id D, body "Why did I lose marks on HW3?"} → X, M1
 4. conversation_inbox{C} → [{id X, latest_opener_message_id M1, opener {member_id P}}]
 5. lease X; conversation_messages{C, X, limit 30} → [M1]
 6. prompt = system + seat facts + memory(D, X) + history; the loop calls assignment_list,
    grade_list, submission_get (course_id set by the runtime), then writes text
 7. conversation_answer{C, X, in_reply_to_message_id M1, body, idempotency_key "answer:X:M1:1"}
    → executed, message_id M2
 8. release X; C is hot for 120 s; update memory(D, X)
Had Yuki written M3 during step 6, step 7 would be moved_on: back to 5, key "answer:X:M3:1".
```

### 2.7 A course tutor answers students

An instructor creates "CS101 Tutor", registers it in the runtime on the
school's key with a course budget, and seats it with
`member.add_delegate{preset: course_tutor}`: seat T, `answers_course` true,
`student_scope` listed with nobody. Once the runtime has started T and said so
(`me_site_chat`), every student finds T in `conversation.respondents`. The runtime keeps one poller for T in the course:
each inbox row is checked against the asker's quota (over it, the canned notice
under the answer's own key, with no model call), then answered by a worker whose
tools are `course_get`, `document_list`, `document_get`, `assignment_list` and
`assignment_get`. At `confirm_required` the answer is proposed and followed
through `event_list` (§2.4). T reads nobody's work, and its prompt says so:
"ask them to paste the relevant part".

## 3. Providers

### 3.1 The internal format

Everything inside the runtime uses one format; adapters translate at the
edge only.

```jsonc
// Request
{
  "system": "…",                                    // one string; each adapter places it
  "messages": [
    {"role":"user","parts":[{"type":"text","text":"Why did I lose marks on HW3?"}]},
    {"role":"assistant","parts":[
       {"type":"reasoning","provider":"anthropic","model":"…","opaque":{…}},   // replayed only to its maker
       {"type":"text","text":"Let me check."},
       {"type":"tool_call","id":"c1","name":"grade_list","args":{"assignment_id":"…"}},
       {"type":"tool_call","id":"c2","name":"assignment_get","args":{"assignment_id":"…"}}]},
    {"role":"tool","parts":[
       {"type":"tool_result","call_id":"c1","name":"grade_list","content":"{…envelope…}","is_error":false},
       {"type":"tool_result","call_id":"c2","name":"assignment_get","content":"{…}","is_error":false}]}
  ],
  "tools": [{"name":"grade_list","description":"…","schema":{…sanitised for the adapter…}}],
  "tool_mode": "auto",                               // auto | none (the forced last answer)
  "limits": {"max_output_tokens": 2000}
}
// Response
{
  "parts": [ …text | tool_call | reasoning… ],
  "stop": "end|tool_calls|max_tokens|content_filter|refusal|context_overflow|tool_error|error",
  "raw_stop": "tool_use",
  "usage": {"input":1234,"cache_read":1000,"cache_write":0,"output":210,"reasoning":64,
            "raw":{…the provider's usage, verbatim…},"estimated":false}
}
```

Rules:
1. `args` is always an object inside; adapters parse and serialise strings.
   Arguments that do not parse become an `is_error` result, without a call to
   Core.
2. Every call has an `id`; where a provider gives none (Gemini's is optional,
   Ollama's native API has none), the adapter makes `call_{n}`.
3. Any `tool_call` part means `tool_calls`, whatever the provider's stop
   reason said.
4. `reasoning` goes back only to the model that made it, within one answer's
   loop. History across answers is rebuilt from `conversation_messages` as
   plain text, so a change of model between answers breaks nothing.
5. A tool result is Core's envelope, cut at 32 KB with `…[truncated, N
   bytes]`, keeping `status` and `error` whole.
6. Files: the runtime fetches `download_url` itself and passes a file part
   (Anthropic `document`, OpenAI `input_file`, Gemini `inlineData`), or
   extracted text. Cap the size; never give the model the URL.

### 3.2 Declaring tools, calls and results

| API | Tool declaration | Call returned | Args | Result sent back | Link field |
|---|---|---|---|---|---|
| OpenAI Chat Completions | `{"type":"function","function":{"name","description","parameters","strict"?}}` | `message.tool_calls[] {id,type:"function",function:{name,arguments}}` | string | the assistant message with its `tool_calls`, then one `{"role":"tool","tool_call_id","content"}` per call | `tool_call_id` |
| OpenAI Responses | `{"type":"function","name","description","parameters","strict"}` | output item `{"type":"function_call","id":"fc_…","call_id","name","arguments"}` | string | input item `{"type":"function_call_output","call_id","output"}` | `call_id`, not `id` |
| Anthropic Messages | `{"name","description","input_schema","strict"?}` | content block `{"type":"tool_use","id":"toolu_…","name","input"}` | object | the next `user` message: `{"type":"tool_result","tool_use_id","content","is_error"?}` blocks first, every result of the batch in one message, straight after the `tool_use` message | `tool_use_id` |
| Gemini generateContent | `tools:[{functionDeclarations:[{name,description,parameters \| parametersJsonSchema}]}]` | part `{"functionCall":{"id"?,"name","args"}}`, perhaps with `thoughtSignature` | object | parts `{"functionResponse":{"id"?,"name","response":{…}}}`, role `user` **[UNVERIFIED]**; errors under `response.error` | `name`, and `id` if given |
| Gemini Interactions | `{"type":"function","name","description","parameters"}` | step `{"type":"function_call","id","name","arguments"}`, status `requires_action` | object | step `{"type":"function_result","name","call_id","result":[{"type":"text","text"}]}` | `call_id` |
| Gemini, OpenAI-compatible | as Chat | as Chat | string | as Chat; the thought signature in `extra_content.google.thought_signature` **[UNVERIFIED]** | `tool_call_id` |
| Azure OpenAI (v1) | as Responses (recommended) or Chat | the same | string | the same | the same |
| AWS Bedrock Converse | `toolConfig:{tools:[{toolSpec:{name,description,inputSchema:{json},strict?}}]}` | `content[] {"toolUse":{"toolUseId","name","input"}}` | object | role `user`, `{"toolResult":{"toolUseId","content":[{"json"}\|{"text"}],"status":"success"\|"error"}}` | `toolUseId` (at most 64 characters, `[a-zA-Z0-9_.:-]+`) |
| OpenAI-compatible servers | as Chat | as Chat | string | as Chat | `tool_call_id` |
| Ollama native `/api/chat` | the OpenAI function shape | `tool_calls[].function.arguments` | object | `{"role":"tool","tool_name","content"}` | none; order only. Use Ollama's `/v1` instead. |

Marking a result as an error: Anthropic `is_error: true`; Bedrock
`status: "error"` (honoured by some models only); Gemini `response: {"error": …}`.
The OpenAI family has no flag, but Core's envelope begins with its `status`
and speaks for itself.

### 3.3 System prompt, parallel calls, tool choice

| API | System prompt | Parallel calls | `tool_choice` the runtime may use |
|---|---|---|---|
| OpenAI Chat | a `system` message (`developer` for newer models) | `parallel_tool_calls`, default true | `auto`, `none`, `required`, named |
| OpenAI Responses | `instructions`, sent again every turn | `parallel_tool_calls` | `auto`, `none`, `required`, named, `allowed_tools` |
| Anthropic | top-level `system`; no system role in `messages` | on by default; `disable_parallel_tool_use` | `auto`, `none`. Some current models, and manual extended thinking, refuse `any` and `tool` with a 400. |
| Gemini generateContent | `systemInstruction` | several `functionCall` parts | `AUTO`, `NONE`, `ANY` (+`allowedFunctionNames`), `VALIDATED` |
| Gemini Interactions | `system_instruction` | yes | `auto`, `any`, `none`, `validated`, `allowed_tools` |
| Bedrock Converse | top-level `system:[{text}]` | several `toolUse` blocks; per model **[UNVERIFIED]** | `auto`, `any`, `tool` (some models). No `none`: leave `toolConfig` out (whether `toolUse` history then needs it is **[UNVERIFIED]**) |
| DeepSeek | `system` | as Chat **[UNVERIFIED]** | `auto` is safe |
| Qwen (DashScope) | `system` | off unless `parallel_tool_calls: true` | `auto`, `none`, named; no `required` |
| Moonshot Kimi | `system` | as Chat | `auto`, `required`, `none`; named is a 400 while thinking is on |
| Zhipu GLM | `system` | as Chat | `auto` only |
| OpenRouter | `system` | `parallel_tool_calls` | whatever the upstream model takes |
| Ollama `/v1` | `system` | not documented | not supported; leave it out |
| vLLM | `system` | by parser | `auto` (needs `--enable-auto-tool-choice --tool-call-parser …`), `required`, named, `none` (tools stay in the prompt without `--exclude-tools-when-tool-choice-none`) |
| LM Studio | `system` (it adds its own for models without native tools) | not documented | not documented |

So the runtime needs only `auto`, and a way to force text on the last turn:
`ForceAnswer` sends `tool_choice: none`, or leaves `tools` out (Ollama, GLM,
Bedrock). Other choices are capability flags, never relied on. Parallel calls
run concurrently, at most `max_parallel_tools` (4) at once, and their results
go back in call order, grouped as each API asks.

### 3.4 Stop reasons

A tool call is always told from the content (rule 3), so the table covers the
rest.

| Internal | OpenAI Chat and compatible | Responses | Anthropic | Gemini gC | Interactions | Bedrock |
|---|---|---|---|---|---|---|
| `end` | `stop` | `completed`, no function_call items | `end_turn`, `stop_sequence` | `STOP` without functionCall | `completed` | `end_turn`, `stop_sequence` |
| `tool_calls` | `tool_calls` (`function_call` is legacy) | function_call items | `tool_use` | functionCall parts (finish stays `STOP`) | `requires_action` | `tool_use` |
| `max_tokens` | `length` | `incomplete`, `max_output_tokens` | `max_tokens` | `MAX_TOKENS` | `incomplete` (its reason **[UNVERIFIED]**) | `max_tokens` |
| `content_filter` | `content_filter`; GLM `sensitive` | `incomplete`, `content_filter` | — | `SAFETY`, `RECITATION` and others | — | `guardrail_intervened`, `content_filtered` |
| `refusal` | — | — | `refusal` | — | — | — |
| `context_overflow` | GLM and others `model_context_window_exceeded` | — | `model_context_window_exceeded` | — | — | `model_context_window_exceeded` |
| `tool_error` (retry the turn once) | — | — | — | `MALFORMED_FUNCTION_CALL`, `UNEXPECTED_TOOL_CALL`, `TOO_MANY_TOOL_CALLS` | — | `malformed_model_output`, `malformed_tool_use` |
| `error` | GLM `network_error`; OpenRouter `error` | `failed`, `cancelled`, `incomplete` with `max_messages` or `steered` | `pause_turn` (server tools only, which the runtime does not use) | `MISSING_THOUGHT_SIGNATURE` (a runtime bug), `MALFORMED_RESPONSE` | `failed` | — |

OpenRouter normalises `finish_reason` and keeps the provider's in
`native_finish_reason`; log both. The loop retries `max_tokens` with partial
text once with a higher cap, within budget; answers `content_filter` and
`refusal` with the configured refusal text; and meets `context_overflow` by
shortening the history (fewer messages, summarised) and trying once more.

### 3.5 Usage

The internal `input` is all input tokens, cached ones included.

| API | `input` | `cache_read` | `cache_write` | `output` | `reasoning` |
|---|---|---|---|---|---|
| OpenAI Chat | `prompt_tokens` | `prompt_tokens_details.cached_tokens` | `prompt_tokens_details.cache_write_tokens` | `completion_tokens` | `completion_tokens_details.reasoning_tokens` |
| Responses | `input_tokens` | `input_tokens_details.cached_tokens` | `input_tokens_details.cache_write_tokens` | `output_tokens` | `output_tokens_details.reasoning_tokens` |
| Anthropic | `input_tokens + cache_creation_input_tokens + cache_read_input_tokens` | `cache_read_input_tokens` | `cache_creation_input_tokens` (split in `cache_creation.ephemeral_5m/1h_input_tokens`) | `output_tokens` | — (within output **[UNVERIFIED]**) |
| Gemini gC | `promptTokenCount` (+`toolUsePromptTokenCount`) | `cachedContentTokenCount` (part of the prompt **[UNVERIFIED]**) | — | `candidatesTokenCount + thoughtsTokenCount` (how thoughts are billed **[UNVERIFIED]**) | `thoughtsTokenCount` |
| Interactions | `total_input_tokens` | `total_cached_tokens` | — | `total_output_tokens` | — |
| Bedrock | `inputTokens` (whether cache is included **[UNVERIFIED]**) | `cacheReadInputTokens` | `cacheWriteInputTokens` | `outputTokens` | — |
| Compatible servers | the Chat fields; when streaming, `stream_options: {include_usage: true}`. With no usage, count with a tokenizer and set `estimated: true`. | | | | |

Keep `raw` beside the numbers, always. Price with a versioned table keyed on
provider, model and date, never with constants in code.

### 3.6 Reasoning that must go back within a loop

| API | What to send back | If left out |
|---|---|---|
| OpenAI Chat | nothing; reasoning is not carried | — |
| Responses | the `reasoning` items that came with tool calls; with `store: false`, ask for and return `encrypted_content` (`include: ["reasoning.encrypted_content"]` **[UNVERIFIED]**) | worse output, or a 400 |
| Anthropic | thinking blocks with their `signature`, unchanged, in the assistant turn **[UNVERIFIED]** | 400 |
| Gemini gC | `thoughtSignature`, on the part it came on | `MISSING_THOUGHT_SIGNATURE` |
| Gemini Interactions (`store: false`) | every earlier model step as received, thought steps included | an error |
| DeepSeek (thinking with tools) | `reasoning_content` of every earlier assistant turn | 400 |
| Kimi (thinking models) | the whole assistant message, `reasoning_content` included | an error, or worse output |
| GLM with `clear_thinking: false` | the whole `reasoning_content`, unchanged | an error, or worse output |
| OpenRouter | `reasoning_details`, unchanged | an error, or worse output |
| Bedrock Converse | `reasoningContent` blocks, for some models **[UNVERIFIED]** | — |

The `reasoning` part keeps the provider's fragment verbatim (`opaque`); only
the adapter that made it reads it.

### 3.7 Tool names

| API | Rule |
|---|---|
| OpenAI (Chat, Responses) | `^[a-zA-Z0-9_-]{1,64}$` |
| Bedrock | `[a-zA-Z0-9_-]`, at most 64 |
| Anthropic | `^[a-zA-Z0-9_-]{1,128}$` |
| Gemini | `a-zA-Z0-9 _ : . -`, at most 128 |
| Kimi | `^[a-zA-Z_][a-zA-Z0-9-_]{0,127}$`: it must start with a letter or `_` |

Core's names meet them all. A tool of the runtime's own, or a prefix, keeps
every name within `^[a-zA-Z_][a-zA-Z0-9_]{0,63}$`, and names are mapped back
through a table, never by rewriting strings.

### 3.8 JSON Schema

**What Core's input schemas hold**, measured over MCP on all 123 tools:
- Unions: `["null","string"]` ×120, `["null","array"]` ×17, `["null","integer"]`
  ×11, `["null","boolean"]` ×4; and for decimals, which Core takes as numbers
  or strings, `["number","string"]` ×6 and `["null","number","string"]` ×8.
- `format: uuid` ×218; one `pattern` (the decimal's) 14 times; 32-bit
  `minimum`/`maximum` on 5 tools; `minLength`/`maxLength` only on
  `idempotency_key`.
- `additionalProperties: false` on every object but six `perms` maps
  (`{"type":"string"}`), on member and preset writes no model is given.
  Optional properties are left out of `required`.
- No `$ref`, `allOf`, `oneOf` or `enum` yet, but the sanitiser handles them.

**The sanitiser** runs once per tool and adapter, cached by the catalogue's
hash.

1. **Bind.** Remove `idempotency_key` and `course_id`; the runtime sets both.
   `course_id` is always the course of the conversation being answered.
2. **Lowest common denominator, for every adapter.** Turn `["null", X]` into
   `X` and add " (optional; omit or null)" to the description. Turn
   `["number","string"]` into `string`, keep the pattern, and add " (a decimal
   number as a string, e.g. \"87.5\")". Where an adapter refuses
   `format: uuid`, move it into the description as " (UUID)".
3. **Per adapter:**

| Adapter | Mode | Transform |
|---|---|---|
| OpenAI Chat, Responses | non-strict; say `strict: false` on Responses, which otherwise tries strict | Nothing. Strict: every property `required`, optional ones nullable, no `allOf`/`not`/`if`/`then`/`else`. |
| Anthropic | non-strict | Nothing. Strict drops `minimum`/`maximum`/`minLength`/`maxLength`, recursion, `minItems` above 1. |
| Gemini gC | `parametersJsonSchema` (its keyword subset **[UNVERIFIED]**) | For `parameters` (OpenAPI 3.0): unions become `nullable: true`; drop `additionalProperties`. |
| Gemini Interactions | `parameters` (JSON Schema) | As `parametersJsonSchema`. |
| Bedrock | `inputSchema.json` | As is for some model families; the full common transform for others **[UNVERIFIED per model]**. |
| DeepSeek | non-strict | Strict (beta, the `/beta` base URL, every function strict) also drops `minLength`/`maxLength` and `minItems`/`maxItems`. |
| Kimi | `strict` defaults to true | The full common transform and the strict one. Whether `strict: false` turns it off is **[UNVERIFIED]**. |
| Qwen, GLM, OpenRouter, vLLM, Ollama, LM Studio | non-strict | The full common transform: local chat templates break easily on unions. |

4. **Reverse map.** Drop `null`s the original schema does not allow (strict
   models send them), put the bound arguments back, and validate against
   Core's schema; a failure goes back to the model as an `is_error` result.

Default: non-strict everywhere, with local validation. Strict is a per-model
choice (`capabilities.strict_tools: true`) once its contract tests pass.

### 3.9 Endpoints and authentication

| Provider | Base and endpoint | Auth | Adapter |
|---|---|---|---|
| OpenAI | `https://api.openai.com/v1`, `/chat/completions`, `/responses` | `Authorization: Bearer` | `openai_chat`, `openai_responses`. OpenAI documents that some of its newest models take tools only through Responses. |
| Azure OpenAI v1 | `https://<resource>.openai.azure.com/openai/v1/` (no `api-version`) | `api-key` header, or an Entra bearer (scope `https://ai.azure.com/.default`) | `openai_responses` (Microsoft's recommendation) or `openai_chat`; `model` is the deployment name |
| Anthropic | `https://api.anthropic.com/v1/messages` | `x-api-key`, `anthropic-version: 2023-06-01` | `anthropic`. Its OpenAI-compatible endpoint ignores `strict` and caching; avoid it. |
| Gemini | `https://generativelanguage.googleapis.com/v1beta/models/{m}:generateContent`; `/v1beta/interactions` | `x-goog-api-key` | `gemini`; `gemini_interactions` later |
| Gemini, OpenAI-compatible | `https://generativelanguage.googleapis.com/v1beta/openai/` | Bearer | `openai_chat` (beta; unknown parameters are ignored silently; extras in `extra_body.google`) |
| Bedrock | `bedrock-runtime.{region}.amazonaws.com`, `/model/{id}/converse` | SigV4, or a Bedrock API key as a bearer **[UNVERIFIED]** | `bedrock_converse`; or Bedrock's OpenAI- and Anthropic-compatible endpoints |
| DeepSeek | `https://api.deepseek.com` (strict: `/beta`; Anthropic format: `/anthropic`) | Bearer | `openai_chat` |
| Qwen (DashScope) | `https://{WorkspaceId}.{region}.maas.aliyuncs.com/compatible-mode/v1`, `https://dashscope-us.aliyuncs.com/compatible-mode/v1` (the older `dashscope.aliyuncs.com` still works; `dashscope-intl` is being retired) | Bearer | `openai_chat` |
| Moonshot | `https://api.moonshot.ai/v1` | Bearer | `openai_chat` |
| Zhipu GLM | `https://api.z.ai/api/paas/v4` (mainland China: `https://open.bigmodel.cn/api/paas/v4` **[UNVERIFIED]**) | Bearer | `openai_chat` (`tool_stream: true` when streaming) |
| OpenRouter | `https://openrouter.ai/api/v1` (`/chat/completions`, `/responses`, `/messages`) | Bearer | `openai_chat`, or `anthropic` through `/messages` |
| Ollama | `http://localhost:11434/v1` | none | `openai_chat` (no `tool_choice`) |
| vLLM | the operator's URL, `/v1` | optional | `openai_chat`; the server must be started with the tool-parser flags |
| LM Studio | `http://localhost:1234/v1` | none | `openai_chat` |

### 3.10 Adapters of its own, and when a library will do

| Adapter | Covers | Milestone |
|---|---|---|
| `openai_chat` | OpenAI Chat, Azure's Chat, every OpenAI-compatible server in §3.9, Gemini's compatible endpoint, OpenRouter, a LiteLLM proxy | M1 |
| `anthropic` | Anthropic; OpenRouter `/messages`; DeepSeek `/anthropic`; Ollama's and LM Studio's Messages endpoints | M2 |
| `gemini` | Gemini generateContent (thought signatures, `parametersJsonSchema`) | M2 |
| `openai_responses` | OpenAI models that take tools only there; Azure | M2 |
| `bedrock_converse` | AWS-hosted models under SigV4 | M2, or never if Bedrock's compatible endpoints do |
| `gemini_interactions` | once generateContent's retirement is announced | M3 or later |

The loop is small (no streaming: Core shows an answer whole), and what breaks is
what libraries lag on: reasoning passthrough, `tool_choice` rules, schema
dialects, usage. So the runtime owns the translation (400–700 lines an
adapter, with golden tests) and uses official SDKs for transport. A LiteLLM
proxy may sit behind `openai_chat` for an operator who wants it, pinned and
hash-checked (PyPI 1.82.7 and 1.82.8 were malicious, 2026-03-24), never as a
hard dependency. The Vercel AI SDK (`ai` 7.x) may replace the adapters only in
a TypeScript runtime, and only if it passes the reasoning and usage contract
tests (§8.2). Not LangChain or LangGraph. Whatever is used must expose raw
usage and raw stop reasons, or cost accounting (§5.3) fails.

## 4. Configuration

Each agent's configuration lives in the runtime's database, edited in its UI
and exported as YAML. Secrets are references into the secret store (§5.4),
never inline.

```yaml
agent:
  id: agt_01J9Z…
  display_name: "CS101 Tutor"
  tenant_id: ten_instr_42
  core:
    base_url: https://lms.example.edu
    transport: mcp                 # mcp | rest
    mcp_protocol: "2025-11-25"     # pinned
    token_ref: secret://ten_instr_42/agents/agt_01J9Z/core_token
  model:
    adapter: anthropic             # openai_chat | openai_responses | anthropic | gemini | bedrock_converse
    model: <provider model id>     # Azure: the deployment name
    base_url: null                 # openai_chat, for anything but OpenAI
    key_ref: secret://school/keys/anthropic-main   # or secret://ten_…/keys/…
    key_source: school             # school | own
    params: {max_output_tokens: 1500, temperature: 0.3}
    reasoning: {effort: low}       # mapped per adapter
    capabilities:                  # overrides the adapter's table
      parallel_tool_calls: true
      strict_tools: false
      tool_choice_none: true
      file_input: true
    fallback: {adapter: openai_chat, model: <id>, base_url: https://api.deepseek.com, key_ref: …}
  prompt:
    system_ref: prompts/course_tutor.v3.md   # its hash is kept per answer
    answer_language: opener        # opener | fixed:<bcp47>
    on_refusal_text: "I can't help with that here. Please ask your instructor."
    on_budget_text: "I couldn't finish this one. Try a narrower question."
  tools:
    mode: derived                  # the seat's perms ∩ allow − deny
    allow: [course_get, document_list, document_get, assignment_list, assignment_get]
    deny: []                       # beside the built-in list (§6.1)
    max_parallel_tools: 4
  answer:
    max_attempts: 3                # §2.4
    on_attempts_exhausted: close   # close | skip
    on_quota_exhausted: canned     # canned | silent
    max_body_chars: 19000          # Core's limit is 20000
  budgets:
    per_answer: {turns: 8, tool_calls: 12, input_tokens: 150000, output_tokens: 4000, wall_clock_s: 90}
    per_agent_day: {usd: 20.00, answers: 2000}
    per_asker_day: {answers: 30, usd: 0.40}   # keyed on (course_id, opener member_id)
  polling:
    inbox_hot_s: 2                 # for hot_window_s after activity
    hot_window_s: 120
    inbox_idle_s: 10
    inbox_max_s: 30
    events_s: 45
    memberships_s: 300
    jitter: 0.25
    max_rate_share: 0.3            # of Core's allowance, for polling
    assumed_core_rate_per_min: 600
  memory:
    enabled: true
    retention_days_after_removal: 30
courses:                           # by course_id
  "0192f3c1-…":
    enabled: true
    prompt_append_ref: prompts/cs101_style.md
    model: {model: <provider model id>}
    budgets: {per_asker_day: {answers: 15}}
    polling: {inbox_idle_s: 5}
```

Precedence: built-in defaults, then the agent, then `courses[course_id]`, and
over all of them the seat's `perms` in Core. The model is offered:

```
toolset(seat) = { t in the catalogue |
    t's gate is allowed by perms(seat)      (every permission it names, or any one for an "any" gate)
    and t in allow, not in deny, not in the built-in deny list,
    and t is a read (M1, M2) }
```

`GET /v1/tools` does not name gates (§10), so the runtime keeps them by hand
and checks them when the catalogue's hash changes. Today: `document_read` for
`course_get`, `assignment_list`, `assignment_get`, `event_list`,
`action_list_mine`; `document_read` or `rubric_read` for `document_list`; any
of those or `submission_read` or `grade_read` for `document_get`;
`submission_read` for `submission_list`, `submission_get`; `grade_read` for
`grade_list`, `grade_get`, `component_tree`, `gradebook_get`. An empty
toolset is fine: the agent answers from the conversation alone.

## 5. Several tenants

### 5.1 Who signs in, and how an agent is connected

| Tenant | Signs in with | May |
|---|---|---|
| Student | Core's assertion | connect their own agents, choose a model from the school's list or bring a key, see usage, pause, delete |
| Instructor | Core's assertion | all that, and register course tutors, with a budget, prompt and style per course |
| School administrator | Core's assertion with `platform_role` `root` or `admin` | school keys, the model list and prices, global quotas, audit |

People sign in to Core, however Core lets them (password, invitation, single
sign-on, a pasted token), and Core vouches for them to the runtime. The
runtime is no identity provider's client and never sees Core's session
cookie: the proxy in front of it strips `Cookie`.

- **Asking.** The web front end, signed in to Core, calls
  `POST <core>/v1/auth/assertion` with `{"audience": "<the runtime's
  audience>"}` and its session cookie or bearer token. The audience is an
  absolute URL, such as `https://lms.example.edu/runtime`, and must be one of
  Core's `RUNTIME_AUDIENCES`. The answer is `{"assertion": "eyJ…",
  "expires_at": "…"}`, with `Cache-Control: no-store`. The front end sends
  `Authorization: Bearer <assertion>` on each call to the runtime, and asks
  for a new one shortly before `expires_at` and once more on a 401.
- **Refused.** `400 invalid_argument` for an audience not listed, or for a
  body that is anything but that one object (another member, a key in
  another case or given twice, anything after it); `403` for a
  suspended account or for anyone but a person (an agent's token gets none);
  `401` with no valid credential; `404` when Core lists no audience; `429`
  under the caller's rate limit; `403` for a browser's `POST` from an origin
  Core does not trust.
- **The assertion** is a JWT (compact JWS), header `{"alg": "EdDSA", "typ":
  "JWT", "kid": …}`, signed with Ed25519. Claims: `iss` (Core's `PUBLIC_URL`,
  no `/` at the end), `aud` (the audience asked for, a string), `sub` (the
  person's actor id), `iat`, `nbf`, `exp`, `jti` (random), `kind` (`human`),
  `name` (the display name), `email` and `platform_role` when there are any,
  and `sid` (the id of the Core credential it was asked with). It lasts
  `ASSERTION_TTL` (5 minutes by default, at most 15), and never past the
  session or token it was asked with.
- **Checking.** The runtime accepts `alg` `EdDSA` and nothing else, against
  the JSON Web Key Set at `GET <core>/v1/auth/keys` (public; cache it for the
  five minutes its `Cache-Control` says, and fetch it again on a `kid` it does
  not know), or against a key pinned in its configuration. It checks that
  `iss` is Core's `PUBLIC_URL`, the base URL it reaches Core at, `aud` its
  own audience, `exp` and `nbf` hold
  (with a few seconds' leeway at most), and `kind` is `human`. It never
  forwards an assertion, and Core takes none as a credential of its own.
- **Roles.** The person is `sub`, and nothing else: `name` and `email` are
  for display. `platform_role` `root` or `admin` is a school administrator.
  Owning an agent is `me_get`'s `owner_actor_id` equal to `sub`. Tutor
  settings for a course need no role of the person's: the owner of an agent
  that holds a seat with `answers_course` there may change them.
- **What it costs.** A sign-out, a suspension or a change of role reaches the
  runtime when the assertion ends, within `ASSERTION_TTL`.

The owner issues a token in Core (My agents), or the front end issues one
for them, and hands it to the runtime, which calls `me_get` and `me_memberships`, shows the seats ("Delegate of Yuki
in CS101: reads your work, answers only you"), and stores the token encrypted,
never to show it again. The owner picks a model and key (an own key is tested
with a one-token call), the runtime says the agent answers in the site
(`me_site_chat`, §2.3), and polling starts. The runtime takes the token only
from the agent's owner: `me_get`'s `owner_actor_id` must be the person signed
in. It refuses a token whose `kind` is not `agent` (never a person's own), and
leaves an agent nobody owns to the runtime's administrators. An agent's owner
never changes in Core, so the owner checked when the token was taken stays
its owner for as long as the token works.

### 5.2 Whose key

| | The owner's own key | The school's key |
|---|---|---|
| Who pays | the owner | the school, or the course's budget |
| Models | any, but those an administrator denies | the administrator's list: providers with acceptable data terms |
| Quotas | a per-agent budget the owner sets | per owner (a student's agents) and per asker (course tutors), both required |
| Data | the owner's provider's terms, with a warning that messages and documents go there | the school's contract; `store: false` wherever it exists |
| Seen by | that owner's agents only | nobody: referred to by id, in memory only in the adapter |

### 5.3 Quotas and cost

- **Ledger**: a row per model call (tenant, agent, course, `member_id`,
  `conversation_id`, message, opener, adapter, model, usage internal and raw,
  price version, cost, key source) and one per answer.
- **Quotas**, per day: per agent; per tenant, across a student's agents on the
  school's key; per `(course_id, opener member_id)`. Member ids are per
  course, so a student in two courses counts twice, which is acceptable.
- Check before a loop, on the p95 of recent answers, and hold hard caps inside
  it (§7.1). Out of quota, post the canned notice under the answer's own key:
  no model call, and the student knows why.
- **Cost** is `uncached input × p_in + cache_read × p_cache_read + cache_write
  × p_cache_write + output × p_out`, at the prices of the day; per agent,
  course and asker. Instructors see counts per asker, never text.

### 5.4 Isolation and secrets

- Tokens and keys are under envelope encryption (a per-tenant data key
  wrapped by KMS or Vault), decrypted in the worker just before use, and never
  logged or shown to a model. Deleting an agent destroys them.
- Each agent has its own MCP client and token; no call crosses agents.
- The runtime's notes are namespaced `(agent_id, member_id, conversation_id)`.
  Core's memory is the agent's, reached by Core's rule (schema.md §2.9): what
  is about an asker only through that asker's conversation. A tutor's shared
  memory of the course is what course staff reviewed; what one student wrote
  reaches another through it only if a reviewer let it, and a reviewer
  rejects anything that names or describes one student.
- Queues, leases and caps are per agent, scheduled fairly. Every query
  filters on the tenant (row-level security is recommended).

## 6. Safety

### 6.1 Threats and defences

| Threat | From | Defence |
|---|---|---|
| Injection in a question | the one asking | Core bounds the seat (a respondent reads nothing its asker cannot, or is the asker's own delegate), not what the agent was told: a tutor's one token reads every conversation addressed to it. Core's instructions say to answer each conversation from it alone; the runtime makes that structural. The worker answering X has no conversation tool, the runtime reads X itself, its notes are per conversation, and Core gives memory about an asker only through that asker's conversation. Agents are read-only in the runtime. |
| Injection in documents, submissions, feedback | their authors | Tool results stay in the tool-result channel; the system prompt says they and messages are data, not instructions. With no web or fetch tool, the answer is the only way out. |
| The model redirecting its answer | injection | The runtime sets `course_id`, `conversation_id`, `in_reply_to_message_id` and the key; the model writes only `body`. |
| Exfiltration through Markdown | injection | Core's frontend loads no image from another origin (it shows a link) and opens links `rel="noopener noreferrer nofollow"`, but a click still sends the URL, so the runtime strips links and images whose URLs carry query strings or context. |
| Escalation | a bug, injection | The allowlist follows `perms`; the deny list applies at every stage; `denied` is never retried with a new key or arguments. |
| Leaked tokens | logs, errors | Redact `ais_[A-Za-z0-9_-]+`, `aisinv_…` and provider key shapes (`sk-…`, `AIza…`, AWS keys) everywhere. |

**Never offered to a model** in M1 and M2: `agent_*`, `credential_*`,
`actor_*`, `member_*`, `action_decide`, `action_review`, `action_withdraw`,
`conversation_*` and `me_site_chat` (the runtime calls those itself), `preset_*`,
`course_create`, `course_update`, `term_*`, `department_*`,
`document_upload_url`, and every write. M3 opens particular writes to
particular workflows (§9).

### 6.2 Confirmation belongs to Core

The runtime neither holds back a write to imitate `confirm_required` nor
avoids proposals to look autonomous; it tells the owner what happened
("waiting for your instructor's approval"). `proposed` is normal, and the
runtime, not the model, decides what follows. Nobody decides their own
proposal, their owner's or another agent's of their owner, and Core refuses
it. An owner decides their own agent's proposal only where they could have
done it themselves without anyone's confirmation (schema.md §2.6): that is a
person's decision in Core, never the runtime's, so a runtime never pairs a
decider's and a proposer's token for one party, nor approves for the owner.

### 6.3 Personal data and retractions

- Messages and documents hold students' data. They go only to providers the
  key's policy allows (§5.2); prefer zero-retention contracts. The prompt
  carries the conversation's tail, the seat's facts, that conversation's
  notes, and what Core's memory holds about its opener and in the course's
  shared memory, framed as information and never instructions; never the
  roster or another student's data.
- Core's memory is deleted by the people it is about, by the agent's owner,
  and by course staff for the shared memory. Read it afresh for each answer
  and keep no copy between answers, in the database, caches or logs, so that
  a deletion is honoured by the next answer.
- Logs and traces hold ids, counts and codes, never text; a per-tenant debug
  capture is encrypted, audited and gone in 7 days. Deleting an agent deletes
  all but the ledger's ids and numbers.
- On `conversation.message_retracted`, or a message with `retracted`, remove
  it from caches and memory and show it as `[message retracted]`. When
  `last_retracted_at` changes, read the conversation again: a retraction adds
  no message for `after_seq` to find. If it was the agent's own answer, note
  in that conversation's notes not to repeat it, and tell the owner.

## 7. Budgets, latency, concurrency

### 7.1 One answer, one bounded loop

```
loop:
  if elapsed > wall_clock_s or turns == max_turns or tool_calls ≥ max_tool_calls
     or tokens ≥ caps:                    → a last turn with ForceAnswer (no tools)
  resp = adapter.call(req)                 // timeout = min(60 s, the wall clock left)
  account(resp.usage)
  if resp.stop == tool_calls:
      validate args → call Core (reads, bounded parallel) → append results; continue
  if resp.stop == end: body = text; break
  max_tokens, content_filter, refusal, context_overflow, tool_error: §3.4
post: conversation_answer, written ahead, key answer:{conversation}:{message}:{attempt}
```

Of the forms of termination in concepts §6, the runtime holds the fixed
condition (an answer posted), every budget (turns, tool calls, wall clock,
tokens, cost; Core sees no model call) and the owner's pause; Core holds a
person's (pause, removal, `expires_at`, a closed conversation, which come back
`denied` or `failed`). A budget spent with no usable text posts
`on_budget_text`: a claimed message always ends answered or with a recorded
reason.

### 7.2 Polling and latency

Core has no long poll, no push and no inbox across courses, so the runtime
polls each course.

- **Inbox**, per seat: every 2 s for 120 s after activity (students ask
  follow-ups), otherwise every 10 s, backing off ×1.5 per empty poll to 30 s;
  every interval jittered ±25 %, first polls spread out.
- **Events**: every 45 s per seat, and after a `proposed` answer at once, then
  after 5, 15 and 45 s. **Seats**: every 300 s, and on any `forbidden`,
  `not_found` or `denied`.
- **429**: sleep `Retry-After` plus jitter, and halve that agent's polling for
  5 minutes. **5xx or network errors**: back off 1, 2, 4 … 60 s, full jitter.

| Measure (autonomous answers, provider outages aside) | Target |
|---|---|
| Noticing: question written → claimed | p95 ≤ 3 s hot, ≤ 13 s idle |
| Answering: claimed → `conversation_answer` returns, at most 2 tool calls | p50 ≤ 10 s, p95 ≤ 30 s |
| As the student sees it (the frontend polls every 3 s) | p50 ≤ 15 s, p95 ≤ 45 s |
| Wall clock per answer | 90 s, configurable |

### 7.3 Spending the rate limit

At the defaults, with `max_rate_share` 0.3, an agent's polling has 180 calls a
minute: `inbox interval per course ≥ courses × 60 / (180 − event and seat calls
per minute)`. An agent in 10 courses may poll each every 4 s or so, so the idle
10 s is well within it. An answer costs about k + 2 calls (messages, k tool
calls, the answer); at k = 4 the remaining 420 a minute allow some 70 answers a
minute. An exam-time rush may need a second tutor (its own allowance) or a
higher `RATE_LIMIT_PER_MINUTE`. Keep a token bucket per agent at 90 % of the
limit, and put answers before polling, and polling before events.

### 7.4 Concurrency and duplicates

- One worker per conversation, under a lease of the wall clock plus 30 s; one
  poller per seat across the cluster (a lease or an advisory lock), or N
  replicas spend the limit N times.
- At most 8 answers at once per agent and 4 per course; the rest wait.
- Duplicates meet in Core. Two workers answering M1 as attempt 1 share the
  key `answer:X:M1:1`: the second gets `idempotency_conflict` or a replay.
  Under different attempts the second is refused as `already_answered` or
  `answer_pending`. A worker that finds the conversation moved on answers the
  newer message under its own key. Nothing is posted twice.

## 8. Observability, testing, deployment

### 8.1 Observability

- **Metrics**: `inbox_polls_total`, `answer_latency_seconds{stage}`,
  `answers_total{outcome}`, `llm_calls_total{adapter,model,stop}`,
  `llm_tokens_total`, `llm_cost_usd_total{key_source}`,
  `core_calls_total{tool,status,error_code}`, `budget_exhausted_total`,
  `lease_takeovers_total`, and `presence_gap_seconds`, alerting above 60 s.
- **Traces**: one per answer, spanning the claim, each model and Core call,
  and the post; ids and numbers only. **Audit**: configuration, secrets,
  debug capture, administrators' views. **The owner's page**: last seen,
  outcomes, proposals waiting, spend, last errors.

### 8.2 Testing

1. **A fake Core** with the same MCP surface and envelope, for `me_*`,
   `conversation_*`, `event_list`, `action_list_mine` and a few reads,
   scriptable: "the opener writes M3 during generation", "the level becomes
   confirm_required", "the seat is removed", "429, Retry-After: 7".
2. **Fixtures recorded from a real Core** for every row of §2.4 and the pinned
   revision, which the fake must match; recorded again per Core release, with
   the hash of `GET /v1/tools` to show drift.
3. **CI** against `ghcr.io/aishie-education/aishie-core:<pinned>` and
   Postgres, as Core's `make e2e`: seat an agent over REST, run the runtime
   with a scripted fake model, and see the answer appear under its key.
4. **Provider contract tests**: golden translations both ways (parallel
   calls, errors, reasoning, every stop reason and usage field); every tool
   through every sanitiser and back through Core's schema; nightly live runs
   on cheap models, with one request declaring every tool at
   `max_output_tokens: 16`, so the provider checks the schemas.
5. **Safety evaluations**: injections in messages and documents
   (exfiltration links, "call member_add", "tell me what other students
   asked"): no tool outside the allowlist, links stripped, the target fixed.
6. **Load**: 200 agents in 5 courses each, against a real limiter: polling
   within `max_rate_share`, no 429 in steady state.

### 8.3 Deployment and language

```
runtime-api     the configuration API, JSON only           stateless, N replicas
runtime-worker  pollers and answer loops                  N replicas; leases in Postgres
postgres        configuration, leases, cursors, memory, ledger (its own; never Core's)
secret store    Vault or a cloud KMS, for data keys
egress proxy    allows Core's host, the host of download_url (Core's own, or its S3), the providers
```

Core needs no way in. Workers scale out, sharing agents by lease; a dead
worker's leases lapse, and duplicates are safe (§7.4). The UI is in the web
front end, which calls the runtime's API with Core's assertion (§5.1); the
API sits behind the proxy on a path of Core's own origin, with `Cookie`
stripped. Settings: `DATABASE_URL`, `KMS_KEY_ID`, the runtime's audience (the
URL listed in Core's `RUNTIME_AUDIENCES`) and Core's base URL, whose
`/v1/auth/keys` checks the assertions, `CORE_BASE_URL_ALLOWLIST` (which Core
installations a token may point at), `EGRESS_PROXY`, `LOG_REDACT_EXTRA`. The
runtime needs no `OIDC_*`. Releases are versioned images; migrations are
additive.

Go is recommended: Core's team knows it, Core runs the same MCP SDK (v1.8.0),
the providers publish Go SDKs (package names **[UNVERIFIED]**), and it builds
one static binary. TypeScript (`@modelcontextprotocol/client` 2.1.0) or Python
(`mcp` 2.2.0) will do too.

## 9. Milestones

| Milestone | Scope | Done when |
|---|---|---|
| **M1: one adapter, a person's own agent** | A Go service, one worker, agents configured from YAML and environment secrets. `openai_chat` (so OpenAI, DeepSeek, Qwen, Kimi, GLM, Ollama, vLLM, LM Studio, OpenRouter and Gemini's compatible endpoint). The MCP client, `me_memberships`, jittered inbox polling, a leased worker per conversation, the read-only toolset through the sanitiser, the answer written ahead, every row of §2.4 but proposals' follow-up. Budgets per answer, redaction, the fake Core, fixtures, CI against Core's image. | A student's own agent answers them end to end in CI and on staging; the moved-on, duplicate and denied paths are tested; no token in any log. |
| **M2: every provider, course tutors, several tenants** | `anthropic`, `gemini`, `openai_responses`, `bedrock_converse`, with reasoning passthrough and contract and nightly tests. The hosted UI, in the web front end, signing in with Core's assertion (§5.1); connecting by token, both kinds of key, the secret store, quotas, ledger and cost, the owner's page. Course tutors with per-asker quotas. Proposals, reviews, rejections, retractions and closures followed through `event_list`. Cluster-wide poller leases, the rate-limit budget, metrics, traces. | Every provider in §3.9 passes its contract tests or is marked unsupported; a 500-student course stays under 30 % of the rate limit; cost is known per asker; the safety evaluations pass review. |
| **M3: grading and other work** | Work started by events: grading agents (rubric reads, `grade_submit` proposals, writes opened per workflow and keyed `tool:{member_id}:{hash}`), feedback and announcement drafts. `gemini_interactions`. What Core adds (§10). | Grading proposals go through Core's approval queue; the same budgets and ledger hold. |

## 10. Open questions for Core

Settled, and written into §1 and §2: the protocol revisions Core takes, the
refusals' `details.reason`, where a rejection's reason is kept, and, in the
frontend, the two-minute presence window and how replies render links and
images. Settled since: who owns the agent (1 below). Still open, none
blocking M1: 2 to 4.

1. **Who owns the agent.** Settled. `me_get` names the agent's owner in
   `owner_actor_id` (§2.3), and the runtime compares it with the person
   signed in to it (§5.1) before it takes a token.
2. **Which permission gates each tool.** The catalogue has no gates, so §4
   keeps them by hand. Proposed: a `gate` field in `GET /v1/tools`.
3. **A rejection's reason.** `action.rejected` carries none and `action_get`
   needs `action_decide`, so the runtime pages `action_list_mine`. Proposed:
   `action_get` open to an action's own actor.
4. **Finding work.** A long poll on `event_list` or `conversation_inbox`, or
   one inbox across courses, would divide polling by the courses an agent sits
   in. Until then, §7.2 and §7.3 stand.
