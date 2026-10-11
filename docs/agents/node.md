# Tachi — Node Handbook (split out of `.tachi.md`)

Read this **before touching anything under `node/`**, before adding a tool or a reminder that talks to another
machine, and when two processes disagree about who is hosting. `node/` is the one place where a request
arrives from a machine nobody is watching, runs a real agent turn, and must survive the caller hanging up —
so its invariants are about what a boundary has to outlive: a disconnect, a parked turn, a compaction, a
`kill -9`, a second process starting.

`.tachi.md` is injected WHOLE into the first message of every session, so it keeps only the conventions that
apply anywhere in the repo and points here for the rest. **New node lessons belong in THIS file, not back in
`.tachi.md`** — and the rules that ride with that file (freshness in the same turn, convention-not-history,
split + index when a subject grows) apply to this split too.

## Contents — read the section you need, not the file

| Section | Read it when |
| --- | --- |
| **Start or attach, and the two listeners** | you touch startup, `node.lock`, `local.json`, the handshake, or a listen address |
| **The inbound turn — the order IS the guarantee** | you touch `runner.go`, `executor.go`, `tasks.go` or the admission layer |
| **A turn that spans two calls (parking)** | you touch AskUser on the inbound side, `park.go`, `question_timeout` / `max_parked`, or cancel |
| **The sending side** | you touch `remoteclient.go`, `peers.go`, `delegations.go`, `sender.go`, `delegatorcache.go`, or a delegation reminder |
| **Holds and reconciliation** | you touch the node's holds, `session/retention.go`, or any session delete |
| **Three SDK facts that shape everything above** | something protocol-shaped fails and the code looks right |
| **Deliberately not done yet** | you are about to add caller auth, a context-close entry, or a managed-session label |

## Start or attach, and the two listeners

- **Which entries host at all**: `StartOrAttachNode` is called by the TUI, the desktop and the channel entry —
  and by nobody else. `-p` deliberately never touches the node service (a one-off pipe run has no "who is
  hosting" and must not squat the machine's identity), and the ACP and `web` entries do not either. So a
  machine with no terminal hosts a node ONLY through the channel entry, which in turn refuses to start with no
  channel enabled: a headless deployment that wants to serve A2A without any IM account needs an inert channel
  to name (`channel/mock`, see [itest.md](itest.md)).
- **The lock decides, never the file.** `StartOrAttachNode` takes `flock.TryAcquire` on
  `<DataDir>/node.lock` (`<BaseDir>/node/`, mode `0700`): success → `host`, `flock.ErrHeld` → `attach`. flock
  is per open FILE DESCRIPTION, so two descriptors in one process conflict too — which is why concurrent
  starts elect exactly one host. Never delete the lock file: the next process would lock a fresh inode nobody
  contests, and never delete `local.json` on shutdown either (its comment says why) — a stale file is harmless
  because every reader matches `InstanceID` against what actually answers before trusting one field.
- **A held lock with no service is `ErrUnavailable`, not a takeover.** `local.json` (atomic, `0600`, the
  credential inside: `instance_id` / `node_id` / `addr` / `pid` / `api_version` / `token` / `started_at`) is
  read only by a process that FAILED the lock, and must not be rewritten by one that then fails to reach the
  service. Identity is DERIVED, never random: `NodeIDFor` = hex of `sha256(pubkey)[:16]`, so attaching to the
  wrong data dir is `ErrIdentityMismatch`; a different live instance is `ErrInstanceChanged`; `APIVersion`
  (currently `1`) mismatch is `ErrVersionMismatch`. Only `ErrUnavailable` / `ErrUnreachable` /
  `ErrInstanceChanged` are retried — a bad file fails fast instead of waiting out `AttachTimeout`.
- **Two listeners, two surfaces, and two DIFFERENT rules about where they may bind.** The local control
  plane is one route, `GET /local/v1/handshake`, Bearer-checked with `subtle.ConstantTimeCompare`; it returns
  the identity and **never the token**. It is loopback FOREVER (`checkLoopback`) — not because it lacks a
  credential but because it is a way for the other Tachi processes on this machine to reach the one holding
  the lock: nobody else's business, and binding it anywhere else hands that credential's endpoint to the
  network for no gain. **`POST /local/v1/session:end` is the other route**, and it is the machine owner's own
  way to stop an execution that belongs to one session (`runner.EndSessionWork`): it takes a SESSION id — a
  reader has a session, never a task id — resolves it through the binding to the live turn, and resolves the
  task `CANCELED` with a reason, because a stopped turn keeps nothing to say about itself
  (`nodeExecutor.publish`). Nothing about it is a caller's: a caller's cancel is the protocol's, arrives over
  the A2A surface, and belongs to whoever owns the task. Any process on this machine reaches it with
  `node.OpenLocalClient`, which reads what is published and proves the answer comes from that same instance.
  The remote A2A surface is a separate listener with its own mux (`/message:send`,
  `/tasks/...`, the well-known Card path); its address is the operator's (`node.listen`) and
  the remote one may bind ANY address (`checkRemoteListen` no longer restricts it) because it always serves
  TLS with a client certificate off loopback — so a caller is always identified, and who may reach the port is
  the network's business.
- **The certificate IS the node identity, in both directions.** The remote surface serves TLS whenever it is
  non-loopback — the ADDRESS is the switch, one expression, so "encrypted", "asks for a client certificate"
  and "carries the identity into the request" can never disagree (`startA2ASurface`) — with a self-signed
  certificate made from `identity.json`'s ed25519 key (`node/tls.go`). `NodeIDFor` derives the node id from
  that same key, so the id a peer reads out of the certificate is unforgeable: a caller can generate keys all
  day and cannot present one that derives somebody else's id. The caller side remembers what it saw instead of
  trusting it twice — TOFU, in `<BaseDir>/peer-identities.json`, the first key is recorded loudly as UNVERIFIED
  and a later handshake with a different key is refused, naming that file. (The first contact is therefore only
  as trustworthy as the path to it: a mesh that authenticates devices, or loopback. That is the trade, and it
  is why the record says `first_seen`.)
- **This node identifies callers; it does NOT authorise them, and that is a decision rather than a gap.** Any
  client certificate is accepted (`serverTLSConfig`): `checkRemoteListen` no longer restricts the address, so
  "who may call" is the network's business — a mesh, a firewall, or loopback. What is still guaranteed is that
  nothing is anonymous: the handshake REQUIRES a certificate, and without one there is no identity and no
  record of who asked. Re-adding a whitelist is a small, deliberate act where the old `node.callers` was, and
  `TestAnyCertificateIsAccepted` is there so it cannot happen by accident.
- **The identity travels one way and the LABEL the other, and the record keeps them apart.** `CallInterceptor`
  writes the certificate's node id into `CallContext.User` — it has to be the interceptor and not the
  middleware, because the REST transport builds a FRESH `CallContext` per request (`a2asrv.NewCallContext`
  shadows whatever was attached), so a User set outside the request handler is discarded. `callerFrom` — task
  records, the dedupe key, the context-ownership check, List's filtering, Get's ownership rule — is downstream
  of that one value and unchanged. The caller's own NAME (`node.name`) rides in as the private header
  `X-Tachi-Node-Name` and is filed as `taskRecord.SelfReportedName`: **any key can claim any name**, so the
  field's own name says what it is, and the identity beside it is the key. A request cannot name its own
  identity (nothing in it reaches `CallerID`), and a connection without a certificate is refused.
- **Inside `host()` the order is lock → local listener → store → remote surface → publish**, and the store
  must have loaded and reconciled before the remote surface can accept work; every failure path releases the
  lock. At shutdown, take the listeners down **before** releasing the lock, or a caller that finds the lock
  free races an instance that is still answering.

## The inbound turn — the order IS the guarantee

- **Admission runs before any side effect.** The chain is: `busyGuard` in front of the handler → the
  `admission.Before` interceptor (fingerprint over caller/message id/digest; a replay returns the STORED task
  as the result) → the task record is persisted → the executor yields `SUBMITTED`+`WORKING` → `beginTurn` →
  `contextRun` (bind or create the session) → holds → the turn's ctx → `LoadSessionHistory` /
  `RunConversationStream`. Nothing before that point may write the
  session, build a reminder, generate a title, create a checkpoint, fire a hook or call a model. The local
  side mirrors it: `RunConversationStream` / `RunOneOffStream` refuse a managed session before the goroutine
  starts, and `managedSessionWriteRefusal` covers rewind, which has no trusted-execution grant.
- **A refusal of ours is rendered by us.** `google.rpc.Status` plus
  `ErrorInfo{domain: "tachi", reason, metadata}` — because the SDK maps an unrecognised error to
  `500 INTERNAL_ERROR` under the protocol domain, which would present a private reason as a standard one. The
  reasons are the vocabulary callers match on: `CONTEXT_BUSY`, `MESSAGE_ID_REUSED`, `CONTEXT_PARKED`,
  `TASK_BUSY`, `TASK_FINISHED`, `UNKNOWN_TASK`, `NO_WAITING_QUESTION`, `QUESTION_MISMATCH`, `ANSWER_SHAPE`.
- **Parked is not busy.** `INPUT_REQUIRED` / `AUTH_REQUIRED` are excluded from "busy": the next message to
  that context is precisely the answer it is waiting for, so counting it as busy would shut a caller out of
  the question it was just asked. A message carrying `taskId` also skips creation-dedupe — it creates nothing.
- **The record is on disk before the response, and the index never decides.** `<DataDir>/tasks/<id>.json` is
  the authority (`putLocked` = atomic `writeRecord` → map → `reindex` → `writeIndex`); `index.json` is derived
  and rebuilt by scanning, so losing it costs speed, not correctness. Dedupe is keyed on the message id
  (caller included once `CallerID` is filled — which is why a caller identity containing `|` is refused at
  write time); the same id with the same digest returns the FIRST task and runs nothing, while the same id
  with different content is refused with `MESSAGE_ID_REUSED` rather than silently handing back the old task.
- **Startup recovery is a state, not a rewrite.** `resolveInterrupted` fails every non-terminal task — running
  and parked alike — with `interruptReason`, keeping the question in the status. A crashed host does not
  resume work, and nothing pretends it did.
- **A remote turn runs with a NARROWER config than a local one, and the narrowing is one function.** The
  agent is built with `SkipMemoryRecall` (the recall reminder injects THIS machine's memory into whatever
  prompt arrives, and skipping the tools would not stop it) and the run's tool view drops `hostOnlyTools`
  (`RecordMemory` / `MemoryRecall`) whatever `node.tools` says — an operator listing them cannot open the
  host's memory to a caller (§2.3). Both halves live in `node/runner.go`; a reload cannot undo either (the
  recall flag is the one part of `MemoryState` `ReloadMemory` preserves, and the tool view is per-run).
- **Every model call a turn makes is attributable to the task that caused it.** `llm.WithUsageSource` puts
  the origin (`a2a`) plus the task and context ids on the turn's context, so the ledger row carries WHOSE
  work it was while `Kind` keeps saying what the call was FOR (§9.5) — a compaction inside a remote turn is
  still a `compact` row, and the two sums ("what did this remote task cost" / "what does this machine spend
  on titling") stay answerable. Both this and the narrowing above sit in `turnContext` / `toolViewOption`:
  one place each, because they are the two ways a remote turn differs from a local one.

## A turn that spans two calls (parking)

- **The executor returns on `INPUT_REQUIRED`; the live turn stays on our side.** The SDK treats that state as
  final and stops reading the event stream, so a blocking `message/send` ends there. The message that arrives
  with `taskId` re-enters the executor (`continueTask`) and reads the SAME turn's stream. Nothing else ends a
  parked turn — not the HTTP request, not the executor.
- **The turn's ctx must not come from the request.** The SDK runs the executor inside an errgroup and cancels
  that ctx the moment the executor returns — exactly when a parked turn has to keep living. The turn derives
  `context.WithoutCancel(parent)` (keeping trace/caller values, dropping the lifetime); turn, cancellation or
  node shutdown decide when it ends.
- **The question id is the AskUserQuestion tool call id**, published in the status message's
  `metadata.questionId`; answers come back under the same key, and a missing or wrong id is refused with the
  current one handed back so the caller can retry rather than guess. The shape: one pending question → a text
  part; several → a `data` part keyed by the FULL question text (the same shape TUI and desktop use), and an
  unmappable answer is refused — never matched by position.
- **The slot is consumed exactly once, and the answer channel is emptied before publishing.** The agent's
  answer channel buffers one value with no request id, so a leftover would be taken as the next question's
  answer; the node discards it before announcing (`DiscardPendingAskUserAnswer`) and uses the
  returns-whether-it-landed `TryRespondToAskUser` to know the answer was really taken. `parking.take` happens
  before the wake, so two answers cannot both claim the question.
- **Both bounds are configuration, and neither refuses new work.** `question_timeout` (default 30m) ends the
  turn and writes the task `FAILED` with the question kept — `INPUT_REQUIRED` means "a turn in this process is
  waiting for this answer", which stops being true at the deadline. `max_parked` (default 16) fails THAT ASK:
  the model is told it could not ask and finishes with what it has.
- **The stream must always be drained.** A live turn is read by `lt.advance()` and `drainIfIdle` starts at most
  one background reader, because the loop's own session writes and checkpoints happen at the END of the stream.
  Cancel calls `stopTurn` BEFORE yielding `CANCELED` (for a parked turn, that is the only thing that ends it),
  and `stopTurn` → `lt.stop()` + `forgetTurn` drops the slot and the task hold together. The known limit: when
  cancel lands in a parked turn's closing race, the SDK's own concurrent-cancel path can report "not
  cancellable" to the caller while this side has already stopped it — SDK behaviour, not fixed here.

## The sending side

- **The three REST routes are spelled out here on purpose.** The SDK's REST client DISCARDS any
  `google.rpc.ErrorInfo` whose domain is not `a2a-protocol.org`, while our refusals are filed under `tachi` on
  purpose — so a caller that reads them through the SDK cannot tell "refused, safe to retry" from "the task is
  over". The sender therefore speaks the protocol itself (`/message:send`, `/tasks/<id>`, `/tasks/<id>:cancel`,
  media type `application/a2a+json`, the `A2A-Version` header) and parses the `google.rpc.Status` envelope
  itself, which keeps ANY domain.
- **`PeerRefusal` keeps the distinction the SDK throws away.** `Transport` (dial failure, timeout, unreadable
  body) means the peer never considered the request — "unknown" is not "safe to retry". `Domain == "tachi"`
  means the peer's admission layer answered us; a protocol domain means something else did. `NotAccepted()` is
  true only for `CONTEXT_BUSY` / `MESSAGE_ID_REUSED` (or a bare 503/409).
- **An answer is a Task OR a Message, and only one of them is a handle.** `send` returns a `sendResult`
  covering both arms of the spec's `SendMessageResponse` oneOf, because the second is a legitimate answer
  ("a direct Message response for simple interactions that don't require task tracking") — this node used to
  refuse it, which discarded the peer's text AND left the record reading "we do not know whether it was
  accepted": three different stories about one successful exchange. A direct Message is recorded as
  `DirectAnswer` + `completed` + its text with `RemoteTaskID` empty (so `Get`/`Wait` answer from the record
  without a round trip, which is the shape they already had for "nothing to ask the peer about"), and the
  peer's `contextId` is kept when it sent one — that is what lets `continue_from` continue the CONVERSATION
  even though no task exists, while `referenceTaskIds` stays empty (there is no task to reference).
  **`noHandle` keeps the two reasons for an empty `RemoteTaskID` apart in every refusal**: "never accepted,
  outcome unknown" is not "answered directly, no task was created" — one says retry, the other says it is
  already done. Only a stub can produce this shape against us: a Tachi peer always answers with a Task.
- **`SUBMITTED` and `WORKING` are both "in flight"**, and `reply` waits too. The `returnImmediately` response
  often arrives before the SDK has written `WORKING`, so treating `SUBMITTED` as a failure double-sends; and
  answering resumes the same turn, so a handle that says `WORKING` is reporting a state nobody has reached
  yet. Taking the handle and getting the result are two different actions, and the second one always goes
  through the same bounded wait. A wait that times out does not cancel the task.
- **The local record is written BEFORE the request leaves** (`put` then `send`), with `LastKnown.State` left
  unspecified: "we do not know whether it was accepted" is a state the record is allowed to be in, not
  something to guess. The peer's message id is kept so a retry is a replay, not a second task. An unknown id
  is refused and points at `RemoteTask(action="list")`; `find` never guesses a nearby record.
- **A record binds an IDENTITY, not the alias it was made under** (§4.1). `PeerNodeID` is written from the
  verified handshake and `continue_from` compares THAT — so renaming a peer is a rename, while pointing an
  alias at another machine is refused, and the refusal names both node ids. The comparison happens BEFORE the
  message leaves (`verifyPeerIdentity` does a card request for the proof): a check that ran afterwards would
  have already read the wrong machine the conversation. The alias stays as the fallback for a peer this
  machine has no identity for at all (plaintext loopback). Two consequences worth knowing: the displayed name
  comes from the CURRENT config (`Sender.displayName`, resolved through the TOFU record), because a reminder
  naming a peer by an alias the config no longer knows would send the model into a refusal; and each of these
  tests fails if the identity check is removed (the suite pins the behaviour, not the code shape).
- **A continuation may name NO peer at all**, and that is the shape the tool produces: its contract is exactly
  one of `node` / `continue_from` (§8.1's own continuation example has no `node` in it), so the record's
  machine is resolved here, in three steps and cheapest first — the pin record (`currentAliasFor`, no
  request), the alias the record was made under (the everyday case), then ASKING each configured peer which
  identity it is (`verifyPeerIdentity`, one card request each). The third step is the only one that survives a
  RENAME: the pin is keyed by the alias a peer was reached under, so a name the config adopted a minute ago is
  in no pin yet, and the old alias is gone — without it that conversation is unreachable, because the model may
  not name a node alongside `continue_from`. Identity first is also why the ORDER matters: asking the recorded
  alias first turns a rename into "not configured any more" and loses work that is still there. The identity
  comparison itself (above) still happens before the message leaves — this step only says where the record's
  peer is reachable.
- **What comes back from a peer is bounded in CHARACTERS, never mid-character — and the part that does
  not fit is kept on THIS machine.** `boundAnswer` keeps the first `answerExcerptLen` runes
  (`strutil.TruncatePlain`) and spills the whole text to `ToolResultsDir` (`remote_<local id>.txt`), so
  the shortened text names a path the model can actually `ReadFile`. The head is kept, unlike Bash /
  WebFetch / SubAgent which replace their whole output with the pointer: the first characters of a remote
  answer are usually the answer, and the same text is what a reminder renders — a pointer there would
  cost a `ReadFile` per long answer and turn a summary into a file path. The spill filename comes from the
  LOCAL RECORD rather than a timestamp (a name that changed per turn would put a moving line into the
  reminder, which is a prompt-prefix cost on every turn: `docs/agents/prompt-cache.md`), and that also
  makes a re-spill overwrite its own file. A spill that fails (no dir, full disk, permissions) falls back
  to the bare excerpt — a tool call that fails is worse than one that answers with less. **Without the
  spill the remainder was unreachable at any price**: it was decoded here for one instant and dropped, and
  the peer's own copy sits behind the same bound (a `get` shortens it again). This is NOT the forbidden
  thing — a remote ABSOLUTE PATH handed over as if it were locally readable; the file is a copy, on this
  machine, and the note says so. The stored request (`promptExcerptLen`) follows the same character rule;
  a declared node name is cut by BYTES (`strutil.TruncateBytes`), because there the bound really is a byte
  count.
- **An empty `LogicalID` is a SCOPE, not a wildcard.** It is what a sessionless run (`-p`) files under, so a
  script's later runs see their own delegations and no real conversation's; `Sender.RemoteTasks` always filters
  by it, and that filter is unconditional — without it a `list` would show every task this machine ever ran,
  which is both useless and a leak between conversations.
- **The delegator is per entry point, rebuilt when the peer set changes, and a FAILED open is not cached** (a
  transient problem heals itself). The cache key is name+URL+**note**, because a stale description of an
  unreachable node is worse than a rebuild. No peers → `nil` delegator → no tools and no reminders. A peer
  Card lookup caches the FAILURE too (5 min), so a down node does not cost a timeout every turn, and an
  unreachable peer is silence rather than an error — the reminder is where the caller finds out.
- **The gate is who attaches.** Both delegation tools are run-level extras (`agent.WithExtraTools`) and are
  never put in the registry, so cron and ambient cannot reach them structurally — no deny-list to keep in
  sync. Four entries attach them (TUI user turn, desktop user turn, channel directed turn, `-p`); `/compact`
  and the one-off `--commit` / `--review` runs do not.

## Holds and reconciliation

- **One kind of hold, and one reason: an execution is running.** `reason=task`, no deadline, withdrawn at
  the terminal state. A remote conversation's session gets NO retention window of its own — it lives and dies
  by the session store's ordinary lifecycle (count-based cleanup, pins, an explicit delete), the same as a
  conversation the reader had. The order is session exists → hold on disk → context binding written → loop
  starts, so a session that loses to a cleanup fails visibly instead of running unprotected.
- **The previous rule, and why it went.** A context used to hold its session for `node.context_retention`
  (168h) from its last use, on top of the task's hold. It was dropped: the window has no standing in the A2A
  spec (`contextId` is an opaque grouping id, nothing more), and it made the retention layer a second clock
  beside the session store's — two answers to "how long does this live" and two sets of delete rules. What it
  cost: the owner's delete is now the only way to reclaim a conversation nobody wants, and a sender that comes
  back after a cleanup gets an explicit failure (never a silent new conversation) that may now arrive earlier.
- **Results do not live in the session.** Artifacts and task states are under `<DataDir>/tasks/`, so a
  reclaimed or compacted session cannot take away a result the node still promises to serve.
- **Compaction moves the protection, it does not drop it.** `FinalizeCompact` notifies the host before the old
  session's own bookkeeping is cleared; the node moves the running task's hold to the new session, persists the
  binding, and only then releases the ref that moved.
- **Startup reconciles leftovers**: each hold is released if its task is terminal or gone, and a hold
  carrying the retired `context` reason (`session.ReasonContextRetired` — a conversation kept for a retention
  window, which this version does not do) is released too, so a data dir written by an older version converges
  on the one reason there is. With no deadline on a hold, this owner is the ONLY thing that can withdraw a
  leftover one. A reason the code has never heard of is different: KEPT and reported, because releasing it
  would drop a session another version is holding.
- **`Close` waits for every turn's stream to be read to the end** (bounded by `shutdownGrace = 3s`), because
  that end is where the loop writes its session and checkpoints. A drain that times out is expected; closing
  never rewrites task records — the next start resolves those as interrupted.

## Three SDK facts that shape everything above

1. **The domain filter on `ErrorInfo`, on both ends.** The client keeps only `a2a-protocol.org`; the server
   renders an unrecognised error as `INTERNAL_ERROR` under that same domain. Hence: we spell the routes
   ourselves, and the guard renders our refusals itself. Private reasons never travel through the SDK's error
   mapping.
2. **`INPUT_REQUIRED` is final to the SDK** (it stops reading the event stream), while the SDK's own
   `Terminal()` does NOT include it. The node's wait settles on terminal OR `INPUT_REQUIRED` OR
   `AUTH_REQUIRED`; the executor must return so the parking resumption can happen; and `AUTH_REQUIRED` is not
   used for tool approval at all.
3. **Task content is cloned through gob on its way into the store.** Only builtin or registered types may sit
   in an `any` slot — handing it `[]tools.Question` fails the WHOLE event with
   `gob: type not registered for interface` and the task ends up `FAILED`. The SDK registers `map[string]any`
   and `[]any`, so the structured part is built by hand from those.

## Deliberately not done yet

Each of these is a decision, not an oversight — the design's ordering is fixed, only the wiring is missing:

- **Answering a simple ask with a `Message` instead of a `Task`.** The receiving side always yields a Task
  first, so a peer always gets a handle. The spec names this strategy (**task-generating agents**: "always
  respond with `Task` objects … This avoids the `Task`-versus-`Message` decision, but it creates completed
  task objects even for simple interactions") and that last clause is the point here: the record IS the
  feature — the audit trail, the retention hold, the usage attribution, the cross-entry reference, the
  answer that survives the caller hanging up all hang off the task. Three things would have to change
  together, and one of them is a hard rule rather than a preference:
  - **the fork is committed at the FIRST yield and there is no going back** (`internal/taskupdate`:
    `message not allowed after task was stored`, and any event before a task or message is
    `first event must be a Task or a message`). So the choice can never follow the turn's outcome; it can
    only be made from the request and a node policy, before the loop runs — a *guess* about whether the
    work will stay simple. (the protocol forum's own hybrid example decides with a router; its "dynamic upgrade"
    variant races the work against a timer and only then yields one of the two — and that window blocks
    the caller, since nothing has been sent yet.)
  - **a `Message` cannot ask a question** (`INPUT_REQUIRED`/`AUTH_REQUIRED` are Task states; the spec's
    taxonomy says message-only agents "cannot support interrupted states"), which is our whole §7.4 chain.
  - a node that sometimes answers with one and sometimes with the other makes every CALLER branch, and the
    message-only branch cannot be waited on, cancelled or answered into.
  So a third-party peer that does this is handled on the SENDING side (see "The sending side"), and this
  node's own answer shape stays single.
- **An explicit "close this context" entry point.** Its callers would be "the sender says it is done with this
  conversation" and "the owner cleans up by hand". **The owner's half is now answered a different way** — an
  explicit delete no longer respects a conversation at all, only a running execution, so the reader who wants
  this session gone deletes it (`session/retention.go`). That leaves the SENDER's half, which needs something
  a peer can call and now matters MORE: with no retention window, nothing on this side reclaims a conversation
  because it went quiet — its session waits for the ordinary count-based cleanup. The semantics are already
  fixed (refuse new continuations, refuse when a task is still running, release last); only the wiring is
  missing.
- **Cleaning up context bindings.** The bindings under `<DataDir>/node/contexts/` are only ever written — one
  small JSON per context this node has seen, growing with the conversations, not with anything that gets
  collected. The retention window never cleaned them either, but it let you say "the live ones are inside the
  window". The binding is worth keeping (it is what makes `continue_from` recognise its own conversation, and
  what tells a delete whose session this is), so the fix is a cleanup that follows the session, not an expiry.
- **Contention tests** (create/compact/delete re-entry, another process taking the coordination lock while a
  node is online). The lock order is fixed, written in comments and in the design; the tests wait for a real
  concurrent scenario to drive them.
- **File `Part`s** (`raw` / `url`) and an artifact download endpoint, with the SSRF check the fetch-back
  direction needs (§6.4). `url` genuinely waits for authentication — a download handle has to be
  authenticated and expiring, and an open one would be the same mistake as an open agent endpoint.
- **A whitelist, if a deployment wants one.** Today any certificate is accepted and the network decides who
  may reach the port. A cluster that wants the sharper rule puts it back where `node.callers` was: the handshake
  already hands over a node id, so the check is a membership test in `verifyCallerCertificate` and nothing else
  moves.
- **Showing a peer's NAME on a record held here.** The session a remote turn lands in already records
  `source_node_id` — the identity out of the caller's certificate, written when the node opens the
  conversation and carried to a compacted successor exactly like `Kind` and `SkipDream` — so the fact is on
  disk, and nothing displays it yet. Turning it into something readable means looking that id up in THIS
  machine's peer directory (`node/knownpeers.go`); the caller's self-reported name is never used for it, since
  any key can claim any name. Two things to know before building it: that directory is written by OUTBOUND
  contact, so a node that only ever receives has pinned nobody and every id resolves to "unknown" (a correct
  answer, but the common one); and the wording has to stay one spelling of one fact across the three
  frontends, the way the `Kind` label already does.

- **Reminders in `-p`.** A one-off run has `DisableSystemReminders` by construction; it gets references from
  `RemoteTask(action="list")` or from the peer names in a refusal instead.
- **Cross-entry completion notification** and idle auto-continuation. A finished remote task is stored, and
  the reminder shows it in the conversation that owns it; nobody is pushed a message in another window yet,
  and no window's drained queue can consume an event another window would then miss.

## Where a managed session SHOWS UP (all three frontends have this now)

A session a subsystem owns is labelled wherever a reader meets it — the rule itself is enforced at the
agent's entry (`agent.Managed`), and everything below is the courtesy that keeps a reader from walking into a
refusal:

- **The row**: `tui/session_selector.go` (and the `@`-picker row, `tui/at_file.go`'s `managedRowSuffix`),
  `web/frontend/src/lib/managed.ts`, the desktop sidebar's `.session-tag.is-managed`. One spelling of one
  fact, and it comes from `session.Session.Kind` in every case.
- **TUI refuses to switch into one** (`loadSession`, before the current session is ended — switching first
  and letting the agent refuse the opening turn would end the reader's own conversation to show an error).
- **Desktop opens it read-only**: `SessionInfo.Kind` travels, the transcript pane shows a banner and the
  composer is REPLACED by the reason (`.composer-readonly`) rather than disabled — a greyed-out field still
  invites a click. Smoke: `managed-session`.
- **The web console is read-only by construction**, so the badge is the whole of its obligation there.
- The desktop's `Kind` field is a binding: after changing `SessionInfo`, regenerate the wails bindings.

Related: [`.tachi.md`](../../.tachi.md), [session-store.md](session-store.md) (holds, the cleanup decision and
its lock), [compact.md](compact.md) (what must travel with a compacted session),
[itest.md](itest.md) (the `itest/run` delegation spec and the harness's peer support).
