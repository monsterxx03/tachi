// The composer: everything between the user's hands and a running turn — the text they are
// typing, the @-file picker and the "/" palette that assist it, the queue of messages typed
// while a turn is already running, and the answers to a question the agent is parked on.
//
// Those four are one unit because of one fact: what the user is *about to say* becomes a
// turn through a single decision — send it now, queue it for the next steer point, or
// dispatch it as a slash command (see `route`). Keeping that decision next to the queue it
// feeds is what stops the two from drifting apart; the queue is also the state that has to
// follow its own conversation (keyed by session, moved on auto-compaction).

import { useCallback, useEffect, useLayoutEffect, useRef, useState, type RefObject } from 'react'
import { Events } from '@wailsio/runtime'
import { AgentService } from '../bindings/github.com/monsterxx03/tachi/desktop'
import type { Question } from '../bindings/github.com/monsterxx03/tachi/agent/tools'
import {
  AT_MAX_RESULTS,
  AT_SEARCH_DEBOUNCE_MS,
  type AtMatch,
  type AtPickerState,
  type Message,
  type PermissionDecision,
  type PermissionRequest,
} from './types'
import type { AgentMatchVO, CommandVO, SessionMatchVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import type { OneOffRun } from './agentEvents'
import {
  HASH_REF_MARK,
  HASH_AGENT_PREFIX,
  HASH_SESSION_PREFIX,
  atRefAt,
  countAtRefs,
  hashNamespace,
  imeActive,
  insertRefText,
  listNav,
  replaceRefText,
} from './lib'
import { isMod } from './platform'
import { finishNotice } from './transcript'
import { AtFilePicker, CommandPicker } from './components'
import { oneOffDoneLabel } from './oneoff'

// The input box's height.
//
// The box HUGS its text: with nothing in it the box is one line tall (the stylesheet's
// min-height, mirrored by COMPOSER_INPUT_MIN_H, so "dragged back down" and "never dragged" are
// the same look), and it grows with what is typed up to COMPOSER_INPUT_MAX_H — the stylesheet's
// max-height — where the text starts to scroll instead. A textarea does not size itself, so that
// measurement is one layout pass per keystroke (see the effect in useComposer).
//
// The READER can override all of it by dragging the box's top edge; their number then wins, and
// its ceiling is a FRACTION of the window rather than a pixel count: the box may grow to be
// comfortable, never to swallow the conversation it is written into, and that ratio is what
// holds on a small screen too.
//
// The reader's own number is not persisted, deliberately — unlike the side panel's width, which
// is a layout choice the reader makes once. The box's height is about the message being written
// now; what the box does on its own is about the same message.
const COMPOSER_INPUT_MIN_H = 24
const COMPOSER_INPUT_MAX_H = 200
const COMPOSER_INPUT_MAX_FRACTION = 0.6
const COMPOSER_INPUT_KEY_STEP = 24

function composerInputMaxH(): number {
  if (typeof window === 'undefined') return COMPOSER_INPUT_MIN_H * 4
  return Math.max(COMPOSER_INPUT_MIN_H * 2, Math.round(window.innerHeight * COMPOSER_INPUT_MAX_FRACTION))
}

// sessionAtMatch maps a backend session match into a popup row. The TITLE is what
// the row reads and the REFERENCE is what gets inserted; both are the backend's
// call (which is the side that expands the reference again), and the date is the
// row's label of which conversation this is — a session has no path to show.
function sessionAtMatch(s: SessionMatchVO): AtMatch {
  return {
    path: s.title || '(untitled)',
    isDir: false,
    ref: s.ref,
    kind: 'session',
    date: s.date,
    workingDir: s.workingDir,
  }
}

// agentAtMatch maps a backend node match into a popup row. Everything it shows is
// already worded by the backend (idLabel, reach): the TUI shows the same two facts about the same
// node, and this must not become the place where they start being phrased differently.
function agentAtMatch(a: AgentMatchVO): AtMatch {
  return {
    path: a.alias,
    isDir: false,
    ref: a.ref,
    kind: 'agent',
    idLabel: a.idLabel,
    reach: a.reach,
    note: a.note,
  }
}

// HASH_PREFIX_CANDIDATES is the picker's opening list for '#': the namespaces themselves, offered
// the moment the mark is typed so the spelling does not have to be remembered. They are built here
// rather than fetched, because they are two constants and a request would arrive after the keystroke
// that asked for them. The words mirror agentref.PrefixCandidates (the TUI's copy): one fact, one
// sentence, two renderers.
const HASH_PREFIX_CANDIDATES = [
  { ref: HASH_REF_MARK + HASH_AGENT_PREFIX, note: 'agent：另一台机器上的 Tachi 节点，可把活派过去' },
  { ref: HASH_REF_MARK + HASH_SESSION_PREFIX, note: 'session：另一段对话（只引用它的位置，不内联全文）' },
]

// hashPrefixMatches narrows those two rows by what has been typed after the mark. Picking one OPENS
// that namespace instead of finishing the reference (a 'prefix' row): the popup stays up on the new
// prefix, which is what makes "#" → "#a:" → the node list one gesture.
function hashPrefixMatches(query: string): AtMatch[] {
  const typed = HASH_REF_MARK + query
  return HASH_PREFIX_CANDIDATES.filter((c) => c.ref.startsWith(typed)).map((c) => ({
    path: c.ref,
    isDir: false,
    ref: c.ref,
    kind: 'prefix' as const,
    note: c.note,
  }))
}

export type ComposerDeps = {
  currentId: string
  // A turn is already running in the session on screen, so text belongs in the queue rather
  // than in a second, competing turn.
  running: boolean
  // The transcript store (useTranscript.ts): what a send writes into.
  transcript: {
    updateSession: (sid: string, fn: (list: Message[]) => Message[]) => void
    applyToSession: (sid: string, role: 'user' | 'assistant', fn: (m: Message) => Message, openIfMissing?: boolean) => void
    injectSteerVisual: (sid: string, text: string, ts: string) => void
    sealRunningSegment: (sid: string) => void
    markRunning: (sid: string, running: boolean) => void
    refreshRunning: () => void
    // holdLocal/releaseLocal: the bubbles this composer places on its own, held against a page
    // that was read before them (see useTranscript's comment) — a send that races a compaction
    // switch's re-read must not lose the reader's own message. releaseLocal runs when the turn the
    // send started is over.
    holdLocal: (sid: string, messages: Message[]) => void
    releaseLocal: (sid: string) => void
  }
  scrollToBottom: (force?: boolean) => void
  // Esc in the composer hands focus to the message area (the Vim-like keys live there).
  chatRef: RefObject<HTMLDivElement | null>
}

export function useComposer(deps: ComposerDeps) {
  const { currentId, running, transcript, scrollToBottom, chatRef } = deps
  const { updateSession, applyToSession, injectSteerVisual, sealRunningSegment, markRunning, refreshRunning,
    holdLocal, releaseLocal } = transcript

  const [input, setInput] = useState('')
  const [sendingNow, setSendingNow] = useState(false)
  // Why the last pasted image was not stored ("" when the last paste was fine or there was none).
  // A paste failure has no bubble to land in — unlike a refused message, nothing was sent — so it
  // gets a line of its own above the box rather than being swallowed.
  const [pasteError, setPasteError] = useState('')
  // The voice button's own four states, and the one line it may need to say.
  //
  // The state is NOT computed here: whether the microphone is actually open is a fact only the
  // backend has (it owns the engine and the watchdog), so the button DRAWS what `voice:state`
  // pushes. A click only asks for a change (see toggleVoice). voiceAvailable is asked once —
  // whether this machine can transcribe at all does not change while the app runs.
  const [voiceState, setVoiceState] = useState<VoiceUiState>('idle')
  const [voiceAvailable, setVoiceAvailable] = useState(false)
  const [voiceNotice, setVoiceNotice] = useState('')
  // IME composition state. Some WebKit builds fire compositionend BEFORE the Enter keydown
  // that commits the candidate, so the ref is cleared on the next macrotask — that keeps the
  // guard active for the committing Enter without swallowing a later, genuine Enter-to-send.
  const composingRef = useRef(false)
  const composerRef = useRef<HTMLTextAreaElement>(null)
  // The assistant bubble the last slash command opened, waiting to be closed: a side-channel
  // command's output goes to the panel, so this is what its one-line anchor is written into.
  const commandRef = useRef<{ sid: string; id: string } | null>(null)

  // ── @-file completion ─────────────────────────────────────────────────────
  // Typing "@" opens a fuzzy picker over the session's working directory. Accepting a match
  // splices a reference into the text; the reference is expanded backend-side at turn start
  // (agent/atfile), so the transcript keeps showing the raw text the user typed. The trigger
  // rule in atRefAt mirrors agent/atfile.IsRefBoundary — the popup must offer exactly what
  // the backend will expand.
  const [at, setAt] = useState<AtPickerState | null>(null)
  // Slash commands the backend supports (fetched once: the set is static per build) and the
  // "/" palette's highlight. The palette is derived from the input below rather than stored,
  // so there is no way for it to go stale.
  const [cmdList, setCmdList] = useState<CommandVO[]>([])
  const [cmdDismissed, setCmdDismissed] = useState<string | null>(null)
  const [cmdIdx, setCmdIdx] = useState(0)
  // Query of the last scheduled search — null (not "") means "none yet": the empty query is a
  // real query (it lists the working directory), so "" cannot double as the sentinel or the
  // very first "@" would never be searched.
  const atQueryRef = useRef<string | null>(null)
  const atSeqRef = useRef(0) // stale-response guard
  const atTimerRef = useRef<number | null>(null)
  // inputRef mirrors the composer text for event listeners (Wails file drops) that must not
  // re-subscribe on every keystroke.
  const inputRef = useRef(input)
  useEffect(() => { inputRef.current = input }, [input])
  // atStateRef mirrors the picker state for the same reason (see the file-drop listener: a
  // drop with the picker open must replace the reference being typed, not append after it).
  const atStateRef = useRef<AtPickerState | null>(null)
  useEffect(() => { atStateRef.current = at }, [at])

  const closeAt = useCallback(() => {
    atQueryRef.current = null
    atSeqRef.current++
    if (atTimerRef.current !== null) {
      window.clearTimeout(atTimerRef.current)
      atTimerRef.current = null
    }
    setAt(null)
  }, [])

  // scheduleAtSearch runs the (debounced) backend search for a reference being typed. The MARK says
  // which kind of question it is and, for '#', the query's prefix says which namespace: '@' asks for
  // paths, "#a:" for this machine's nodes, "#s:" for other conversations. Every namespace answers
  // with the SAME row shape, because every row is just a reference to be inserted.
  const scheduleAtSearch = useCallback((mark: string, query: string) => {
    const sid = currentId
    atQueryRef.current = query
    if (atTimerRef.current !== null) window.clearTimeout(atTimerRef.current)
    const seq = ++atSeqRef.current
    atTimerRef.current = window.setTimeout(() => {
      atTimerRef.current = null
      // Clear the spinner on every path — a rejected (or even synchronously throwing) call
      // must not leave the picker stuck on "搜索中…".
      const applyResult = (items: AtMatch[]) => {
        if (seq !== atSeqRef.current) return // superseded by a newer query
        setAt((p) => (p && p.query === query ? { ...p, items, idx: 0, loading: false } : p))
      }
      // The namespaces themselves are local: no round trip for two constants.
      if (mark === HASH_REF_MARK && hashNamespace(query).ns === 'candidates') {
        applyResult(hashPrefixMatches(query))
        return
      }
      try {
        let search: Promise<AtMatch[]>
        if (mark === HASH_REF_MARK) {
          const { ns, rest } = hashNamespace(query)
          search = ns === 'sessions'
            ? AgentService.SearchSessions(sid, rest, AT_MAX_RESULTS).then((res) => (res || []).map(sessionAtMatch))
            : AgentService.SearchAgents(rest, AT_MAX_RESULTS).then((res) => (res || []).map(agentAtMatch))
        } else {
          search = AgentService.SearchFiles(sid, query, AT_MAX_RESULTS).then((res) => res || [])
        }
        search.then(applyResult).catch(() => applyResult([]))
      } catch {
        applyResult([])
      }
    }, AT_SEARCH_DEBOUNCE_MS)
  }, [currentId])

  // syncAtRef recomputes the picker from the composer's value + caret. Called on every
  // text/selection change, which is also how it closes: no reference under the caret means
  // no picker.
  const syncAtRef = useCallback((value: string, caret: number) => {
    const ref = atRefAt(value, caret)
    if (!ref) {
      closeAt()
      return
    }
    // A '#' that opens no namespace is ordinary text ("#include", "#123"): no picker over it, so
    // typing a heading or an issue number is not interrupted by a popup that could only come back
    // empty.
    if (ref.mark === HASH_REF_MARK && hashNamespace(ref.query).ns === 'none') {
      closeAt()
      return
    }
    // Typing '#' is the moment to start asking the nodes: the answer costs a round trip per node,
    // and the typing that follows is what the reader spends it on. It is fire-and-forget — the rows
    // that come back later carry whatever the probe found by then.
    if (ref.mark === HASH_REF_MARK) {
      try {
        void Promise.resolve(AgentService.WarmAgents()).catch(() => {})
      } catch {
        /* the picker still works: reachability just stays "检查中…" */
      }
    }
    const count = countAtRefs(value)
    setAt((prev) => (prev && prev.start === ref.start && prev.mark === ref.mark && prev.query === ref.query
      ? { ...prev, count }
      : { start: ref.start, mark: ref.mark, query: ref.query, items: [], idx: 0, loading: true, count }))
    if (atQueryRef.current !== ref.query) scheduleAtSearch(ref.mark, ref.query)
  }, [closeAt, scheduleAtSearch])

  // insertAtCaret splices text into the composer at the caret, keeping the focus and the
  // caret after the inserted text.
  const insertAtCaret = useCallback((text: string, trailingSpace: boolean, resync: boolean) => {
    const caret = composerRef.current?.selectionStart ?? inputRef.current.length
    const ins = insertRefText(inputRef.current, caret, text, trailingSpace)
    setInput(ins.value)
    requestAnimationFrame(() => {
      const node = composerRef.current
      if (!node) return
      node.focus()
      node.setSelectionRange(ins.caret, ins.caret)
      if (resync) syncAtRef(ins.value, ins.caret)
    })
  }, [syncAtRef])

  // A pasted image is BYTES with no path, so the backend stores it first (under the session's own
  // directory) and answers with the @-reference to splice in at the caret — the same route a
  // dropped file takes, which is also the route an image already travels to the model (@-file
  // expansion turns it into a multi-modal part). Pasting TEXT is left alone: this only claims the
  // gesture when the clipboard actually carries an image, so nothing about ordinary paste changes.
  const onPaste = useCallback((e: React.ClipboardEvent<HTMLTextAreaElement>) => {
    const items = e.clipboardData?.items
    if (!items || items.length === 0) return
    const images: File[] = []
    for (let i = 0; i < items.length; i++) {
      const item = items[i]
      if (item.kind !== 'file' || !item.type.startsWith('image/')) continue
      const f = item.getAsFile()
      if (f) images.push(f)
    }
    if (images.length === 0) return
    // Claimed: otherwise the browser would also paste the image's textual form (a file name, or
    // nothing), and the reference is what belongs in the box.
    e.preventDefault()
    void (async () => {
      for (const f of images) {
        try {
          const url = await readAsDataURL(f)
          const res = await AgentService.SavePastedImage(currentId, f.type, url.slice(url.indexOf(',') + 1))
          if (res?.ref) {
            closeAt()
            insertAtCaret(res.ref, true, false)
            continue
          }
          setPasteError(res?.error || '粘贴的图片没有存下来')
        } catch {
          setPasteError('粘贴的图片没有存下来')
        }
      }
    })()
  }, [currentId, closeAt, insertAtCaret])

  // The notice is transient: it reports a paste that did not happen, and the reader either retries
  // (their screenshot is still on the clipboard) or moves on.
  useEffect(() => {
    if (!pasteError) return
    const t = window.setTimeout(() => setPasteError(''), PASTE_NOTICE_MS)
    return () => window.clearTimeout(t)
  }, [pasteError])

  // acceptAt REPLACES the reference being typed with the picked row's own reference
  // — it must not splice at the caret, or the "#query" the user was typing survives
  // and the text ends up with a stray "#" (counted as a second reference). A
  // directory keeps the picker open on the new prefix so the user can drill in; a namespace row
  // ("#a:") does the same thing one level up — it OPENS a namespace and the popup moves into it. A
  // session or node row is a leaf (its reference is the whole thing, and what it expands to
  // is the backend's business).
  const acceptAt = useCallback((match?: AtMatch) => {
    if (!match || !at) return
    const dir = !!match.isDir
    const opens = dir || match.kind === 'prefix'
    // The backend decides the reference form (relative under the primary root,
    // absolute under an additional one, "#a:<alias>" for a node, "#s:<id>" for a conversation);
    // Path is only what the row displays. A row that OPENS a namespace gets no trailing space: the
    // reference is not finished, and a space would end it on the spot.
    const text = (match.ref || '@' + match.path) + (dir ? '/' : opens ? '' : ' ')
    const ins = replaceRefText(inputRef.current, at.start, text)
    atQueryRef.current = null // whatever follows is a fresh query
    setAt(null)
    setInput(ins.value)
    requestAnimationFrame(() => {
      const node = composerRef.current
      if (!node) return
      node.focus()
      node.setSelectionRange(ins.caret, ins.caret)
      if (opens) syncAtRef(ins.value, ins.caret)
    })
  }, [at, syncAtRef])

  // ── AskUserQuestion ───────────────────────────────────────────────────────
  // The agent parks the turn and waits for answers; the questions arrive as an event and are
  // answered through AgentService.AnswerQuestion (the TUI answers the same channel via
  // RespondToAskUser). Pending questions are kept per session so a background session's
  // question never hijacks the foreground UI — and NOT cleared on session switch: the agent
  // is still parked, so switching back must show the same form.
  const [asks, setAsks] = useState<Record<string, { toolId: string; questions: Question[] }>>({})
  const clearAsk = useCallback((sid: string) => {
    setAsks((prev) => {
      if (!prev[sid]) return prev
      const next = { ...prev }
      delete next[sid]
      return next
    })
  }, [])
  const answer = useCallback((sid: string, answers: Record<string, string> | null) => {
    clearAsk(sid)
    // nil answers = the user declined; the model is told the question went unanswered instead
    // of being handed a made-up choice.
    AgentService.AnswerQuestion(sid, answers, null).catch(() => {})
  }, [clearAsk])
  // answerCurrent is the stable callback the transcript form uses for the session on screen.
  const answerCurrent = useCallback((a: Record<string, string> | null) => answer(currentId, a), [currentId, answer])

  // ── Bash 权限确认 ─────────────────────────────────────────────────────────
  // A bash `ask` rule parks the turn until the user decides in the app; the request arrives
  // as an event and is answered through AgentService.AnswerPermission with the tool call id
  // it names. Same keying as the questions above (per session, NOT cleared on session switch
  // — the agent is still parked), and the same reason: a background session's ask must not
  // hijack the foreground UI, but switching back must still show it.
  //
  // The entry keeps the answer's own state (busy / err) rather than dropping on click: the
  // wait is ended by the BACKEND accepting the answer, and a refused one (the run is gone, the
  // ask was already answered) must stay visible with its reason instead of vanishing into a
  // turn that will never move again.
  type PermEntry = { req: PermissionRequest; busy?: boolean; err?: string }
  const [perms, setPerms] = useState<Record<string, PermEntry>>({})
  const clearPerm = useCallback((sid: string) => {
    setPerms((prev) => {
      if (!prev[sid]) return prev
      const next = { ...prev }
      delete next[sid]
      return next
    })
  }, [])
  const answerPerm = useCallback((sid: string, req: PermissionRequest, decision: PermissionDecision) => {
    setPerms((prev) => {
      const cur = prev[sid]
      // Only ever answer the ask that is on screen: a late click on a replaced card must not
      // mark the new one busy (or clear it when it fails).
      if (!cur || cur.req.toolId !== req.toolId || cur.busy) return prev
      return { ...prev, [sid]: { ...cur, busy: true, err: undefined } }
    })
    AgentService.AnswerPermission(sid, req.toolId, decision)
      .then((res) => {
        if (res === 'ok') {
          clearPerm(sid) // the backend took the decision: nothing is waiting any more
          return
        }
        setPerms((prev) => {
          const cur = prev[sid]
          if (!cur || cur.req.toolId !== req.toolId) return prev
          return { ...prev, [sid]: { req: cur.req, err: res || '回答失败' } }
        })
      })
      .catch(() => {
        setPerms((prev) => {
          const cur = prev[sid]
          if (!cur || cur.req.toolId !== req.toolId) return prev
          return { ...prev, [sid]: { req: cur.req, err: '回答失败（后端不可达）' } }
        })
      })
  }, [clearPerm])
  // answerPermCurrent is the stable callback the transcript form uses for the session on screen.
  const answerPermCurrent = useCallback((decision: PermissionDecision) => {
    const cur = perms[currentId]
    if (cur) answerPerm(currentId, cur.req, decision)
  }, [currentId, perms, answerPerm])

  // ── Pending queue / steer ─────────────────────────────────────────────────
  // Queue ops keep pendingRef in sync so event listeners (which can only see the latest
  // values through refs, not stale render closures) always act on the freshest queue.
  const [pending, setPending] = useState<Record<string, string[]>>({})
  const pendingRef = useRef<Record<string, string[]>>({})
  const enqueuePending = useCallback((sid: string, text: string) => {
    const next = { ...pendingRef.current, [sid]: [...(pendingRef.current[sid] || []), text] }
    pendingRef.current = next
    setPending(next)
  }, [])
  const dropPending = useCallback((sid: string, idx: number) => {
    const list = pendingRef.current[sid] || []
    const next = { ...pendingRef.current, [sid]: list.filter((_, i) => i !== idx) }
    pendingRef.current = next
    setPending(next)
  }, [])
  const clearPending = useCallback((sid: string) => {
    if (!(pendingRef.current[sid] || []).length) return
    const next = { ...pendingRef.current, [sid]: [] }
    pendingRef.current = next
    setPending(next)
  }, [])
  // takePending joins and clears the queue, returning the combined text ("" if empty) — the
  // single drain point for steer injection / send-now / auto-flush.
  const takePending = useCallback((sid: string) => {
    const text = (pendingRef.current[sid] || []).join('\n\n')
    if (text) {
      const next = { ...pendingRef.current, [sid]: [] }
      pendingRef.current = next
      setPending(next)
    }
    return text
  }, [])
  // rekeySessions follows auto-compaction: the queued messages are the conversation's, so
  // they move to the child session with it (the transcript does NOT — see useTranscript).
  const rekeySessions = useCallback((from: string, to: string) => {
    if (!from || !to || from === to) return
    const moveKey = <T,>(rec: Record<string, T>): Record<string, T> => {
      if (!(from in rec)) return rec
      const { [from]: kept, ...rest } = rec
      return { ...rest, [to]: kept }
    }
    setPending(moveKey)
    pendingRef.current = moveKey(pendingRef.current)
    setAsks(moveKey)
    setPerms(moveKey)
  }, [])

  // ── Sending ───────────────────────────────────────────────────────────────
  // sendText appends the user message + a running assistant placeholder and starts a backend
  // turn. Shared by the composer, the queue's "send now" and the auto-flush after a naturally
  // completed turn.
  const sendText = useCallback((raw: string) => {
    const text = raw.trim()
    if (!text) return
    const sid = currentId
    const ts = Date.now()
    const tsStr = new Date().toISOString()
    const placeholder = `a-${ts}`
    const bubble: Message = { id: `u-${ts}`, role: 'user', text, ts: tsStr }
    const waiting: Message = { id: placeholder, role: 'assistant', running: true, parts: [], ts: tsStr }
    updateSession(sid, (prev) => [...prev, bubble, waiting])
    // Held against a page that is already in flight: a compaction switch re-reads the conversation
    // from disk, and this send can land in that window (see useTranscript's holdLocal).
    holdLocal(sid, [bubble, waiting])
    markRunning(sid, true)
    scrollToBottom(true)
    // The backend is told WHICH session this belongs to and answers with a reason when no turn
    // starts (no such session, or one already running). Rendering that reason in the placeholder
    // this call just opened is what keeps a message that never went out from looking like one that
    // did: otherwise the bubble sits at 正在执行 forever, waiting for a reply to a turn nobody is
    // running, with nothing on screen saying so.
    AgentService.SendMessage(sid, text).then((why) => {
      if (!why || why === 'ok') return
      updateSession(sid, (list) => list.map((m) => (
        m.id === placeholder ? { ...finishNotice(m, why), running: false } : m
      )))
      markRunning(sid, false)
    }).catch(() => {
      updateSession(sid, (list) => list.map((m) => (
        m.id === placeholder ? { ...finishNotice(m, '消息没有发出去'), running: false } : m
      )))
      markRunning(sid, false)
    })
  }, [currentId, updateSession, markRunning, scrollToBottom])

  // runCommand sends a slash command. It renders exactly like a message — user bubble plus a
  // running assistant placeholder, so the command's streamed output has somewhere to land —
  // but the backend dispatches it instead of starting a chat turn. A non-empty result means
  // the command never ran (unknown, or the session is busy): that is shown as a notice rather
  // than an empty reply.
  const runCommand = useCallback((text: string) => {
    const sid = currentId
    const ts = Date.now()
    const tsStr = new Date().toISOString()
    // The placeholder is a bubble this composer opened, and it is the composer that closes it:
    // a side-channel command (/review, /commit) sends no content into the conversation, so the
    // bubble has to become one line saying where its output went (see noticeCommandResult).
    // Addressing it by the id remembered here beats "the newest running assistant".
    commandRef.current = { sid, id: `a-${ts}` }
    const bubble: Message = { id: `u-${ts}`, role: 'user', text, ts: tsStr }
    const waiting: Message = { id: `a-${ts}`, role: 'assistant', running: true, parts: [], ts: tsStr }
    updateSession(sid, (prev) => [...prev, bubble, waiting])
    // Held for the same reason a send is: /compact switches the conversation to its child mid-command
    holdLocal(sid, [bubble, waiting])
    markRunning(sid, true)
    scrollToBottom(true)
    const refuse = (label: string) => {
      commandRef.current = null
      applyToSession(sid, 'assistant', (m) => ({ ...finishNotice(m, label), running: false }))
      markRunning(sid, false)
    }
    AgentService.RunCommand(text).then((refusal) => {
      if (refusal) refuse(refusal)
    }).catch(() => refuse('命令执行失败'))
  }, [currentId, updateSession, markRunning, scrollToBottom, applyToSession])

  // noticeCommandResult closes the placeholder a side-channel command opened: its output went
  // to the panel (see agentEvents.ts / oneoff.tsx), so the conversation keeps one line naming
  // what happened and where the rest is. A no-op when the last command was refused (nothing
  // to close) or when the run predates this window.
  const noticeCommandResult = useCallback((run: OneOffRun) => {
    const slot = commandRef.current
    if (!slot) return
    commandRef.current = null
    updateSession(slot.sid, (list) => list.map((m) => (
      m.id === slot.id ? { ...finishNotice(m, oneOffDoneLabel(run)), running: false } : m
    )))
    markRunning(slot.sid, false)
  }, [updateSession, markRunning])

  // route is the single decision about text the user has committed: a leading "/" is a
  // command (the backend owns the list and answers with a notice when it does not know the
  // name); while a turn is running the text queues for the next steer point instead of
  // starting a competing turn; otherwise it starts one.
  const route = useCallback((text: string) => {
    if (text.startsWith('/')) {
      runCommand(text)
      return
    }
    if (running) {
      enqueuePending(currentId, text)
      return
    }
    sendText(text)
  }, [running, currentId, enqueuePending, sendText, runCommand])

  // submit sends text the user did not type into the box — the diff panel's 发给 agent.
  // It rides this route rather than a channel of its own, for the same reason: a busy
  // session has to queue, not refuse.
  const submit = useCallback((text: string) => {
    const t = text.trim()
    if (t) route(t)
  }, [route])

  // 设备上说的那句话。它和 diff 面板的「发给 agent」是同一种东西——一句「用户没说出口
  // 但已经定下来」的文本——所以走同一个 submit，也就自动继承了 route：空闲时开一个
  // turn，在跑时进队列等 steer 点（设计文档 §7）。
  //
  // 后端送来的 payload 只有文本、**没有会话 id**，这是有意的：哪个会话是「当前」的
  // 只有这里知道，所以也由这里决定它归谁（route 用的是 currentId）。
  useEffect(() => {
    const off = Events.On('badge:voice', (event) => {
      const text = (event.data as { text?: string } | undefined)?.text
      if (text) submit(text)
    })
    return () => off()
  }, [submit])

  // ── Voice input (the microphone button) ───────────────────────────────────
  //
  // The SAME transcription half as the badge's voice, a different delivery: the device has no
  // keyboard, so its text becomes a message on the spot (`badge:voice` → submit above); the desktop
  // has one, so this text becomes a DRAFT — spliced into the box at the caret, sent only when the
  // reader presses Enter. A misheard word is cheaper to fix than a sent message is to unsend.
  //
  // Three events, because the three do different things here: state redraws the button, text goes
  // into the box, notice says one line above it. The backend owns the state machine (it owns the
  // microphone and the 60s watchdog that closes an abandoned recording), so a click never guesses.
  useEffect(() => {
    AgentService.VoiceInputAvailable().then(setVoiceAvailable).catch(() => setVoiceAvailable(false))
  }, [])

  useEffect(() => {
    const offState = Events.On('voice:state', (event) => {
      const state = (event.data as { state?: string } | undefined)?.state
      if (state) setVoiceState(state as VoiceUiState)
    })
    const offText = Events.On('voice:text', (event) => {
      const text = (event.data as { text?: string } | undefined)?.text
      // Trail space: dictation is almost always continued by typing (or by starting to type).
      if (text) insertAtCaret(text, true, false)
    })
    const offNotice = Events.On('voice:notice', (event) => {
      const message = (event.data as { message?: string } | undefined)?.message
      if (message) setVoiceNotice(message)
    })
    return () => { offState(); offText(); offNotice() }
  }, [insertAtCaret])

  // toggleVoice is the whole gesture — the button's click and the ⌘⇧M shortcut call the same one.
  // It asks the BACKEND to flip, and the backend decides which way that is: the direction depends on
  // whether the microphone is open, and that is a fact only Go has (the state this component draws
  // arrives over events, which lag by exactly the moment that matters — the first authorization
  // window). A local boolean here would make a second press stop a recording that never started.
  const toggleVoice = useCallback(() => {
    AgentService.ToggleVoiceInput()
      .catch((err: unknown) => setVoiceNotice(err instanceof Error ? err.message : String(err)))
  }, [])

  // A voice notice is transient like the paste one: it reports something about the recording that
  // just ended, and the reader either retries or moves on.
  useEffect(() => {
    if (!voiceNotice) return
    const t = window.setTimeout(() => setVoiceNotice(''), VOICE_NOTICE_MS)
    return () => window.clearTimeout(t)
  }, [voiceNotice])

  // send is the composer's own entry point: it owns the text box, so it clears it first.
  const send = useCallback(() => {
    const text = input.trim()
    if (!text) return
    closeAt()
    setInput('')
    route(text)
  }, [input, closeAt, route])

  // acceptCommand completes the palette's highlighted name into the input, leaving a trailing
  // space so arguments follow naturally. Completing rather than sending is deliberate:
  // "/rev" + Enter should not run the wrong command.
  const acceptCommand = useCallback((cmd?: CommandVO) => {
    if (!cmd) return
    setCmdIdx(0)
    setInput('/' + cmd.name + ' ')
    requestAnimationFrame(() => composerRef.current?.focus())
  }, [])

  // sendPendingNow is the queue's "[立即发送]": interrupt the running turn and send the queued
  // text as the next one.
  //
  // Two things have to be true at once, and the ORDER is what makes them so:
  //   1. the interrupted turn's segment is sealed BEFORE the new message is placed — its
  //      terminal event (error / interrupted) targets "the newest running assistant", so a
  //      message placed first would be the one marked 已停止 (and its own reply dropped);
  //   2. the assistant placeholder is NOT placed here at all — the stream events open it (see
  //      applyToSession's openIfMissing), because the backend starts the next turn while the
  //      stop call is still on its way back, so its first delta can beat this function.
  // Only the user's bubble goes in now: it has to stay above the reply.
  const sendPendingNow = useCallback(async () => {
    const sid = currentId
    const text = takePending(sid)
    if (!text.trim()) return
    setSendingNow(true)

    if (!running) {
      // Nothing to interrupt: the plain send path places both bubbles and owns the IPC.
      sendText(text)
      setSendingNow(false)
      return
    }

    sealRunningSegment(sid)
    const ts = Date.now()
    const bubble: Message = { id: `u-${ts}`, role: 'user', text, ts: new Date(ts).toISOString() }
    updateSession(sid, (prev) => [...prev, bubble])
    holdLocal(sid, [bubble])
    markRunning(sid, true)
    scrollToBottom(true)

    const requeue = () => {
      // Nothing was sent, so the segment we just opened must be closed again — and the text
      // belongs back in the queue rather than in the transcript as if it had gone out.
      sealRunningSegment(sid)
      enqueuePending(sid, text)
      refreshRunning()
    }
    try {
      const ret = await AgentService.StopAndSend(sid, text)
      if (ret && ret !== 'ok') requeue()
    } catch {
      requeue()
    }
    setSendingNow(false)
  }, [currentId, running, takePending, updateSession, enqueuePending, scrollToBottom,
      sealRunningSegment, markRunning, refreshRunning, sendText])

  // answerSteer replies to the agent's steer_check for a session: queued text is drained,
  // shown in the transcript and injected; otherwise the empty string unblocks the agent loop
  // without steering.
  const answerSteer = useCallback((sid: string) => {
    const text = takePending(sid)
    if (!text) {
      AgentService.Steer(sid, '').catch(() => {})
      return
    }
    injectSteerVisual(sid, text, new Date().toISOString())
    AgentService.Steer(sid, text).catch(() => {})
  }, [takePending, injectSteerVisual])

  // ── Subscriptions ─────────────────────────────────────────────────────────
  // A session switch changes the working directory, so any open picker is stale.
  useEffect(() => { closeAt() }, [currentId, closeAt])

  // The slash commands the desktop supports. Fetched once — the set is baked into the build —
  // and used both by the "/" palette and to decide what a lead-in "/" means when sending.
  useEffect(() => {
    AgentService.ListCommands().then((list) => setCmdList(list || [])).catch(() => {})
  }, [])

  // Native file drops arrive from Go (the webview cannot read dropped paths): resolve them
  // into @-references and splice them in at the caret.
  useEffect(() => {
    const off = Events.On('agent:filedrop', (event) => {
      const d = event.data as { paths?: string[] } | undefined
      if (!d?.paths?.length) return
      const paths = d.paths
      ;(async () => {
        try {
          const files = (await AgentService.ResolveDroppedPaths(currentId, paths)) || []
          const refs = files.map((f) => f.ref).filter(Boolean) as string[]
          if (!refs.length) return
          const batch = refs.join(' ')
          const open = atStateRef.current
          closeAt()
          if (open) {
            // Drop while the picker is open: replace the "@query" being typed.
            const ins = replaceRefText(inputRef.current, open.start, batch + ' ')
            setInput(ins.value)
            requestAnimationFrame(() => {
              const node = composerRef.current
              node?.focus()
              node?.setSelectionRange(ins.caret, ins.caret)
            })
            return
          }
          insertAtCaret(batch, true, false)
        } catch { /* ignore */ }
      })()
    })
    return () => off?.()
  }, [currentId, closeAt, insertAtCaret])

  // Questions arrive while the turn is parked; the form renders in the transcript (see
  // AssistantBubble), so pull the view down to it.
  useEffect(() => {
    const off = Events.On('agent:ask', (event) => {
      const d = event.data as { sessionId?: string; toolId?: string; questions?: Question[] } | undefined
      if (!d?.sessionId || !d.questions?.length) return
      setAsks((prev) => ({ ...prev, [d.sessionId as string]: { toolId: d.toolId || '', questions: d.questions as Question[] } }))
      // The form renders in the transcript, so follow it into view — but only while the user is
      // already parked at the bottom: yanking someone who is reading history down to a form
      // would be exactly the kind of interruption this UI should avoid.
      if (d.sessionId === currentId) scrollToBottom()
    })
    return () => off?.()
  }, [currentId, scrollToBottom])

  // 一条等待在别处被回答了——设备。前端自己回答时卡片是它自己清的，但设备走的是
  // 后端那条路直接调回答函数，窗口这边毫不知情：卡片会一直挂着，而且还能再点。
  // 回答成功后端就广播一次，这里按 kind 撤对应的那张。
  useEffect(() => {
    const off = Events.On('agent:ask_answered', (event) => {
      const d = event.data as { sessionId?: string; kind?: string } | undefined
      if (!d?.sessionId) return
      if (d.kind === 'permission') clearPerm(d.sessionId)
      else clearAsk(d.sessionId)
    })
    return () => off?.()
  }, [clearAsk, clearPerm])

  // A permission request arrives while the turn is parked; same placement and same
  // follow-into-view rule as the questions above.
  useEffect(() => {
    const off = Events.On('agent:permission', (event) => {
      const d = event.data as PermissionRequest | undefined
      if (!d?.sessionId || !d.toolId) return
      setPerms((prev) => ({ ...prev, [d.sessionId]: { req: d } }))
      // Only while the user is already parked at the bottom (see the ask listener).
      if (d.sessionId === currentId) scrollToBottom()
    })
    return () => off?.()
  }, [currentId, scrollToBottom])

  // agent:idle fires after a turn goroutine has fully exited (running already reset on the
  // backend). A NATURAL completion auto-sends whatever the user queued while the turn ran
  // (same drain semantics as TUI's TurnComplete); stopped/errored turns leave the queue for
  // the user to review, drop or send manually.
  useEffect(() => {
    const off = Events.On('agent:idle', (event) => {
      const d = event.data as { sessionId: string; reason: string; current?: boolean }
      clearAsk(d.sessionId) // the turn is over: nothing can still be waiting
      clearPerm(d.sessionId) // same fact for a parked permission
      // The read that the switch performed for the COMPACTION window is over by now — anything held
      // for this session either made it into the window or never had a page to land on (see
      // holdLocal). Dropping it here is what keeps the hold from outliving the send it was made for.
      releaseLocal(d.sessionId)
      // `current` comes from the backend (was this the displayed session?) rather than being
      // compared against this closure's currentId: an auto-compaction switch can move the
      // session between the two events, and React state read through a stale closure would
      // then silently drop the queued message.
      if (!d.current || d.reason !== 'complete') return
      const queued = pendingRef.current[d.sessionId] || []
      if (queued.length === 0) return
      sendText(takePending(d.sessionId))
    })
    return () => off?.()
  }, [sendText, takePending, clearAsk, clearPerm, releaseLocal])

  // ── Height (the box hugs its text; the reader drags the top edge to overrule it) ──────
  // inputHeight: the reader's own box height (drag / arrow keys), null while they have not asked
  // for one. measuredH: what the draft itself needs. One number serves both the height and the
  // ceiling in the reader's case: the stylesheet's max-height exists to stop the DEFAULT from
  // eating the transcript, and a reader who deliberately asks for a taller box has already
  // answered that question.
  const [inputHeight, setInputHeight] = useState<number | null>(null)
  const [measuredH, setMeasuredH] = useState<number | null>(null)
  const [heightDragging, setHeightDragging] = useState(false)
  const heightDragRef = useRef<{ y: number; h: number } | null>(null)
  const heightDetachRef = useRef<(() => void) | null>(null)
  // measureInput: how tall the draft itself needs to be, right now — read from the element with
  // the height it is about to be given taken OFF (otherwise the box's own height answers instead
  // of the text's), and put back afterwards so React keeps owning that property (see inputStyle).
  const measureInput = useCallback(() => {
    const el = composerRef.current
    if (!el) return
    const prev = el.style.height
    el.style.height = ''
    const need = el.scrollHeight
    el.style.height = prev
    setMeasuredH(need > COMPOSER_INPUT_MIN_H ? Math.min(need, COMPOSER_INPUT_MAX_H) : null)
  }, [])
  // The composer can be unmounted mid-drag (a session switch re-renders the footer); then
  // nothing would ever remove the window listeners.
  useEffect(() => () => heightDetachRef.current?.(), [])
  // A shrunken window must not leave a box taller than its share of the screen — and the box's
  // own WIDTH moved with it, so the measurement has to be redone too (a draft that wraps onto two
  // lines at the old width may fit in one at the new one).
  useEffect(() => {
    const onResize = () => {
      setInputHeight((h) => (h === null ? h : Math.min(h, composerInputMaxH())))
      measureInput()
    }
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
  }, [measureInput])
  // Measure the draft. A textarea never sizes itself, so without this a long message would type
  // itself out inside a one-line window; with it, the box is as tall as what it holds and there
  // is no dead space under a short one. Layout effect, so the frame that paints the new text
  // already has the box that fits it. The reader's height wins while they have one (a drag is a
  // decision, and it comes back here the moment they drag back to the floor).
  useLayoutEffect(() => {
    if (inputHeight !== null) return
    measureInput()
  }, [input, inputHeight, measureInput])

  const startHeightDrag = (e: React.PointerEvent<HTMLDivElement>) => {
    e.preventDefault()
    // The baseline comes from the DOM, not from a constant: on the first drag the box is whatever
    // the stylesheet made it, and that is the honest starting point.
    const startH = inputHeight ?? composerRef.current?.offsetHeight ?? COMPOSER_INPUT_MIN_H
    heightDragRef.current = { y: e.clientY, h: startH }
    setInputHeight(startH)
    setHeightDragging(true)
    const heightAt = (clientY: number) => {
      const d = heightDragRef.current
      if (!d) return null
      // Up is taller: the handle sits on the TOP edge, so the box grows away from the pointer.
      return Math.max(COMPOSER_INPUT_MIN_H, Math.min(composerInputMaxH(), Math.round(d.h + (d.y - clientY))))
    }
    const move = (ev: PointerEvent) => {
      const h = heightAt(ev.clientY)
      if (h !== null) setInputHeight(h)
    }
    const finish = (ev: PointerEvent) => {
      const h = heightAt(ev.clientY)
      heightDragRef.current = null
      detach()
      setHeightDragging(false)
      // Dragged back down to the floor = "not resized", so the default stays a single value in CSS.
      setInputHeight(h !== null && h > COMPOSER_INPUT_MIN_H ? h : null)
    }
    const detach = () => {
      window.removeEventListener('pointermove', move)
      window.removeEventListener('pointerup', finish)
      window.removeEventListener('pointercancel', finish)
      document.body.classList.remove('is-resizing-y')
      heightDetachRef.current = null
    }
    // Attached synchronously, like the side panel's resizer: React commits the state
    // asynchronously, and a fast drag delivers its first moves before an effect could attach.
    window.addEventListener('pointermove', move)
    window.addEventListener('pointerup', finish)
    window.addEventListener('pointercancel', finish)
    document.body.classList.add('is-resizing-y')
    heightDetachRef.current = detach
  }
  // One step of keyboard height, shared by the handle's ↑↓ and by ⌘⇧↑/↓ inside the field: same step,
  // same floor and ceiling, so the two ways in cannot drift apart. Back at the floor the value returns
  // to null — "not resized" stays ONE value, and that value is the stylesheet's.
  const nudgeHeight = (dir: number) => {
    const from = inputHeight ?? composerRef.current?.offsetHeight ?? COMPOSER_INPUT_MIN_H
    const next = Math.max(COMPOSER_INPUT_MIN_H, Math.min(composerInputMaxH(), from + dir * COMPOSER_INPUT_KEY_STEP))
    setInputHeight(next > COMPOSER_INPUT_MIN_H ? next : null)
  }
  // Arrow keys resize too: the handle is a real focusable separator, not a decoration.
  const onHeightKey = (e: React.KeyboardEvent<HTMLDivElement>) => {
    const dir = e.key === 'ArrowUp' ? 1 : e.key === 'ArrowDown' ? -1 : 0
    if (!dir) return
    e.preventDefault()
    nudgeHeight(dir)
  }
  // Height and max-height together for the reader's number, and only once they have resized;
  // otherwise the measured height (which the stylesheet's own max-height already caps).
  const inputStyle = inputHeight
    ? { height: inputHeight, maxHeight: inputHeight }
    : measuredH
      ? { height: measuredH }
      : undefined

  // ── Focus ─────────────────────────────────────────────────────────────────  // The two halves of the UI hand focus to each other (Esc leaves the composer, a new
  // session enters it), and those callers sit in dependency lists — so both get a stable
  // identity instead of a fresh closure per render, which would make every caller churn.
  const focusInput = useCallback(() => composerRef.current?.focus(), [])
  const focusTranscript = useCallback(() => chatRef.current?.focus(), [chatRef])

  // The "/" palette is DERIVED from the input rather than stored, so it can never disagree
  // with what would actually be dispatched: it is open while the input is a bare command name
  // still being typed (a space means arguments follow, an exact name means it is complete),
  // and Esc dismisses it for the current query only, so typing on reopens it.
  const cmdQuery = input.startsWith('/') && !/\s/.test(input.slice(1)) ? input.slice(1) : null
  const cmdMatches = cmdQuery === null ? [] : cmdList.filter((c) => c.name.startsWith(cmdQuery.toLowerCase()))
  const cmdOpen = cmdQuery !== null && cmdDismissed !== cmdQuery && !cmdList.some((c) => c.name === cmdQuery)

  return {
    // input box
    input, setInput, composerRef, composingRef, syncAtRef, closeAt, focusInput, focusTranscript,
    // input box height (drag / arrow keys; never persisted)
    inputStyle, startHeightDrag, onHeightKey, heightDragging, nudgeHeight,
    // @-picker + "/" palette
    at, setAt, acceptAt, cmdOpen, cmdMatches, cmdIdx, setCmdIdx, cmdQuery, setCmdDismissed, acceptCommand,
    // queue
    pending: pending[currentId] || [], dropPending: (i: number) => dropPending(currentId, i),
    clearPending: () => clearPending(currentId), sendPendingNow, sendingNow, rekeySessions,
    // questions
    ask: asks[currentId] || null, answer, answerCurrent, clearAsk,
    // bash permission (a policy `ask` rule parked the turn on the user)
    perm: perms[currentId] || null, answerPerm: answerPermCurrent, clearPerm,
    // sending
    send, submit, answerSteer, noticeCommandResult,
    // pasted images
    onPaste, pasteError,
    // voice input (the microphone button; the backend owns the state, this reads it)
    voiceState, voiceAvailable, voiceNotice, toggleVoice,
  }
}

// PASTE_NOTICE_MS is how long a failed paste stays on screen: long enough to read one line, short
// enough that the box is clean again by the time the reader has retried the paste.
const PASTE_NOTICE_MS = 5000

// VOICE_NOTICE_MS is the same idea for what a recording has to say ("没有听到内容", "麦克风权限被
// 拒绝…"). Longer than a paste's: the remedy is sometimes "go and change a system setting", so the
// line has to survive being read twice.
const VOICE_NOTICE_MS = 8000

// VoiceUiState 是后端推给按钮的四个状态（desktop/voice.go 的 voiceState）。
//
// idle 什么都没发生；preparing 正在准备（首次使用多半是在等那个授权窗）；recording 麦克风开着，
// 再点一下结束；transcribing 音频到手、识别在跑。
type VoiceUiState = 'idle' | 'preparing' | 'recording' | 'transcribing'

// voiceButtonLabel 是按钮悬停时那一句话（也是它的 aria-label）。
//
// 它随状态变，因为每一步用户要做的动作都不一样——而「首次使用要先去弹窗里点允许」这件事尤其
// 说不出口就没有第二次机会：那个弹窗在系统那一层，页面上没有任何别的地方能提示它。
function voiceButtonLabel(state: VoiceUiState): string {
  switch (state) {
    case 'recording': return '正在录音，再点一下结束（语音输入）'
    case 'preparing': return '正在准备…首次使用请在系统弹窗里点「允许」'
    case 'transcribing': return '正在识别…'
    default: return '语音输入：点一下开始录音'
  }
}

// readAsDataURL reads a clipboard File — a pasted image exists only as bytes, so this is the only
// way to reach them from the page.
function readAsDataURL(f: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const r = new FileReader()
    r.onload = () => resolve(String(r.result || ''))
    r.onerror = () => reject(new Error('read failed'))
    r.readAsDataURL(f)
  })
}

export type ComposerApi = ReturnType<typeof useComposer>

// Composer renders the input row: the pending queue above it, the two pickers and the
// textarea, and the stop/send buttons. It owns no state — `api` is useComposer's return
// value — so what is rendered and what is sent can never disagree. The <footer> around it
// (and the status row that shares that footer) belongs to the caller.
export function Composer({ api, running, onStop, onPageTurn }: { api: ComposerApi; running: boolean; onStop: () => void; onPageTurn: (delta: number) => void }) {
  const { input, setInput, composerRef, composingRef, syncAtRef, closeAt, at, setAt, acceptAt, cmdOpen, cmdMatches, cmdIdx, setCmdIdx, setCmdDismissed, cmdQuery, acceptCommand, pending, dropPending, clearPending, sendPendingNow, sendingNow, send, focusTranscript, inputStyle, startHeightDrag, onHeightKey, heightDragging, nudgeHeight, onPaste, pasteError, voiceState, voiceAvailable, voiceNotice, toggleVoice } = api

  // ONE line above the box, because a paste failure and a recording's complaint can never both be
  // news at the same moment — and two stacked notices would push the conversation up twice.
  const notice = pasteError ? `⚠ ${pasteError}` : voiceNotice
  const voiceLabel = voiceButtonLabel(voiceState)

  return (
    <>
      {pending.length > 0 && (
        <div className="pending-bar">
          <div className="pending-head">
            <span className="pending-title">
              {running ? '⏸ 待发送 · 会在本轮工具调用结束后自动插入' : '待发送消息'}
            </span>
            <span className="pending-actions">
              <button className="pending-send" disabled={sendingNow} onClick={sendPendingNow}>
                {sendingNow ? '正在停止…' : running ? '立即发送 · 打断' : '发送'}
              </button>
              <button className="pending-clear" onClick={clearPending} title="清空待发送队列">清空</button>
            </span>
          </div>
          {pending.map((t, i) => (
            <div className="pending-item" key={`${i}-${t.slice(0, 8)}`}>
              <span className="pending-bullet">·</span>
              <span className="pending-text" title={t}>{t}</span>
              <button className="pending-del" title="撤回该条" onClick={() => dropPending(i)}>✕</button>
            </div>
          ))}
        </div>
      )}
      <div className="composer-box" data-file-drop-target="true">
        {/* The grab strip along the box's top edge: a real focusable separator, and the only way
            the reader says "I need more room to write this than the text itself asks for". It sits
            INSIDE the box — as an overlay on its top padding, so it costs the box no height — and
            it must not swallow the box's own focus (a click on it never focuses the textarea:
            that is what preventDefault is for). */}
        <div className={`composer-resizer${heightDragging ? ' is-dragging' : ''}`} role="separator"
          aria-orientation="horizontal" aria-label="调整输入框高度" tabIndex={0}
          title="向上拖动加高输入框（↑ ↓ 也可以）"
          onPointerDown={startHeightDrag} onKeyDown={onHeightKey} />
        {notice && <div className="paste-notice">{notice}</div>}
        {cmdOpen && (
          <CommandPicker
            items={cmdMatches}
            selected={Math.min(cmdIdx, Math.max(0, cmdMatches.length - 1))}
            onPick={(i) => acceptCommand(cmdMatches[i])}
          />
        )}
        {at && (
          <AtFilePicker
            mark={at.mark}
            query={at.query}
            items={at.items}
            selected={at.idx}
            loading={at.loading}
            refCount={at.count}
            onPick={(i) => acceptAt(at.items[i])}
            onHover={(i) => setAt((p) => (p ? { ...p, idx: i } : p))}
          />
        )}
        {/* One row: the text, and the controls beside it (bottom-aligned). The buttons used to get
            a row of their own under the text — height charged to every message, and the blank it
            left under a one-line draft was the taller half of the box. */}
        <div className="composer-row">
          <div className="composer-input-wrap">
            <textarea className="composer-input" ref={composerRef} rows={1} value={input} style={inputStyle}
              onChange={(e) => { setInput(e.target.value); syncAtRef(e.target.value, e.target.selectionStart ?? e.target.value.length) }}
              onSelect={(e) => { const el = e.currentTarget; syncAtRef(el.value, el.selectionStart ?? el.value.length) }}
              onBlur={() => closeAt()}
              onCompositionStart={() => { composingRef.current = true }}
              onCompositionEnd={() => { window.setTimeout(() => { composingRef.current = false }, 0) }}
              onKeyDown={(e) => {
                const ime = composingRef.current || imeActive(e)
                // ⌃U / ⌃D page the transcript, and they work HERE too — the caret is usually in this
                // box when someone wants to look back up at what was just said. The message area's own
                // handler cannot do it for us: it refuses keys targeted at a field, by design, and a
                // keydown born here never passes through it anyway. So the composer claims the pair and
                // hands it to the same half-page scroll the badge's gesture uses. That takes the two
                // away from WebKit's own text handling in this field (macOS gives ⌃D a caret meaning —
                // delete forward): a key that means different things depending on where the focus is is
                // the worse deal, and paging the conversation is what the pair means everywhere else.
                if (!ime && e.ctrlKey && (e.key === 'u' || e.key === 'd')) {
                  e.preventDefault()
                  onPageTurn(e.key === 'd' ? 1 : -1)
                  return
                }
                // ⌘⇧↑/⌘⇧↓ resize the box from INSIDE the field — the handle's own ↑↓ and the drag stay
                // as they are. Deliberately not a window-level binding: ⌥↑/⌥↓ already walks the diff
                // pane's findings, and a global handler would have to out-guess which pane is focused.
                // Plain ⌘↑/⌘↓ keep the text system's caret gestures (and still select, with ⇧, to the
                // ends of the box).
                if (!ime && isMod(e) && e.shiftKey && (e.key === 'ArrowUp' || e.key === 'ArrowDown')) {
                  e.preventDefault()
                  nudgeHeight(e.key === 'ArrowUp' ? 1 : -1)
                  return
                }
                // While the @-picker is open it owns the navigation keys; an IME composing
                // (candidate selection) owns them first.
                if (!ime && at) {
                  // ↑↓ / Ctrl+N·P move the highlight (see listNav); the picker clamps at the ends.
                  const delta = listNav(e)
                  if (delta) {
                    e.preventDefault()
                    setAt((p) => (p && p.items.length
                      ? { ...p, idx: Math.max(0, Math.min(p.items.length - 1, p.idx + delta)) }
                      : p))
                    return
                  }
                  if (e.key === 'Tab' || (e.key === 'Enter' && !e.shiftKey)) {
                    // Tab/Enter accept the highlighted match; with no match, Enter still sends
                    // (Tab just dismisses the picker).
                    if (at.items.length > 0) { e.preventDefault(); acceptAt(at.items[at.idx]); return }
                    if (e.key === 'Tab') { e.preventDefault(); closeAt(); return }
                  }
                  if (e.key === 'Escape') { e.preventDefault(); closeAt(); return }
                }
                // The "/" palette owns the navigation keys while it is open (the @-picker cannot
                // be open at the same time: one is triggered by "@", the other by a leading "/").
                if (!ime && cmdOpen) {
                  const delta = listNav(e)
                  if (delta) {
                    e.preventDefault()
                    setCmdIdx((i) => Math.max(0, Math.min(cmdMatches.length - 1, i + delta)))
                    return
                  }
                  if (e.key === 'Tab') { e.preventDefault(); acceptCommand(cmdMatches[Math.min(cmdIdx, cmdMatches.length - 1)]); return }
                  if (e.key === 'Escape') { e.preventDefault(); setCmdDismissed(cmdQuery); return }
                  // Enter completes rather than sends while the name is still partial:
                  // "/rev" + Enter must not run the wrong command.
                  if (e.key === 'Enter' && !e.shiftKey && cmdMatches.length > 0) {
                    e.preventDefault()
                    acceptCommand(cmdMatches[Math.min(cmdIdx, cmdMatches.length - 1)])
                    return
                  }
                }
                // Esc hands the focus to the message area (the Vim-like keys live there). Not
                // listed in the shortcut sheet on purpose: it is a focus affordance, not a
                // feature shortcut.
                if (e.key === 'Escape') { e.preventDefault(); focusTranscript(); return }
                if (e.key !== 'Enter' || e.shiftKey) return
                // Enter while an IME is composing confirms the candidate (选词), it must not send
                // the message.
                if (ime) return
                e.preventDefault()
                send()
              }}
              onPaste={onPaste}
              placeholder={running ? '正在运行…输入后 Enter 将加入待发送，在工具间隙自动插入' : '发送消息给 Tachi…（Enter 发送，Shift+Enter 换行，@ 引用文件，#a: 选节点，#s: 选会话）'} />
          </div>
          <div className="composer-actions">
            {/* 语音输入：只在能转写的机器上出现（macOS 26 之前转写那半不存在）。它和发送键并排，
                因为它产出的东西就落在旁边那个输入框里——两件事是同一个动作的两半。 */}
            {voiceAvailable && (
              <button
                className={`mic-btn${voiceState === 'recording' ? ' is-recording' : ''}`}
                onClick={toggleVoice}
                disabled={voiceState === 'preparing' || voiceState === 'transcribing'}
                title={voiceLabel} aria-label={voiceLabel}>
                <svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                  <rect x="9" y="3" width="6" height="11" rx="3" />
                  <path d="M5 11a7 7 0 0 0 14 0" />
                  <line x1="12" y1="18" x2="12" y2="21" />
                </svg>
              </button>
            )}
            {/* The stop control sits in this cluster rather than floating over the input: while a
                turn runs it is one of the two things this box can do (stop it, or queue behind it),
                and floating it over a one-line box was exactly the blank the box carried. */}
            {running && (
              <button className="stop-btn" title="停止生成" onClick={onStop} aria-label="停止生成">
                <svg viewBox="0 0 24 24" width="10" height="10" fill="currentColor"><rect x="7" y="7" width="10" height="10" rx="1.5" /></svg>
              </button>
            )}
            <button className="send-btn" onClick={send} disabled={!input.trim() || sendingNow}>
              <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round"><line x1="5" y1="12" x2="19" y2="12" /><polyline points="12 5 19 12 12 19" /></svg>
              <span>{running ? '排队' : '发送'}</span>
            </button>
          </div>
        </div>
      </div>
    </>
  )
}
