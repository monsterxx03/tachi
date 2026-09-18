import { Fragment, useCallback, useEffect, useMemo, useState, type ReactNode } from 'react'
import { AgentService } from '../bindings/github.com/monsterxx03/tachi/desktop'
import type { SettingFieldVO, SettingsSchemaVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { Caret } from './components'

// The settings page: every editable leaf of the config, grouped, one input each.
//
// Nothing here knows a config KEY. The backend declares what there is (GetSettingsSchema
// walks config's own leaves), what each field is worth (the live value), how it should be
// drawn (`kind`, derived from the value's Go type), whether a restart is the only way to make
// it live (`needsRestart`, derived from the apply table), and where it belongs (`category`,
// from the settings table). This file renders whatever it is handed, so adding a config field
// is a change in Go alone — which is the point of the table.
//
// What it DOES own is the shape of an edit: what the boxes hold, when a row is dirty, what a
// refused save does to the reader's typing.

/** What one input holds. Text for everything except a switch, where the value IS the state —
 *  and text rather than a parsed value so that a half-typed number ("1" on the way to "150")
 *  survives the keystroke, and so a refused save can put the reader's typing back untouched.
 *
 *  null is a STATE, not an empty value: it is 自动 — the leaf is UNSET, which the optional
 *  leaves give a meaning to (`vision: nil` = decide it from the model). It has to stay null all
 *  the way to the file (`vision: null`), which is why it is not spelled as "". */
type Draft = string | boolean | null

/** The value a row shows before anyone touches it — the same shape as a Draft, so "dirty" is
 *  one comparison. */
function fieldDraft(f: SettingFieldVO): Draft {
  // Unset first, and from `isSet` rather than from the value: an unset pointer WITH a default
  // reads as that default, so the value alone cannot tell 自动 from a value somebody chose.
  if (f.optional && !f.isSet) return null
  if (f.kind === 'bool') return f.value === true
  if (f.value === null || f.value === undefined) return ''
  if (f.kind === 'string_list') return Array.isArray(f.value) ? f.value.join('\n') : ''
  return String(f.value)
}

/** parseField turns what a box holds into the value SaveSettings takes.
 *
 *  A number that is not a number is refused HERE rather than sent: the backend would refuse
 *  the batch too (its type table is the authority on what a leaf can hold), but it refuses the
 *  WHOLE batch — so one typo would throw away every other field's edit and every value would
 *  have to be retyped. Catching it locally costs a round trip's worth of surprise. */
function parseField(f: SettingFieldVO, draft: Draft): { value?: any; error?: string } {
  // 自动. The backend turns this into "unset" for the leaves that can take one, and refuses it
  // for any that cannot — which would be a bug on this side, not a value to guess at.
  if (draft === null) return { value: null }
  const text = typeof draft === 'string' ? draft.trim() : ''
  switch (f.kind) {
    case 'bool':
      return { value: draft === true }
    case 'string_list':
      // One per line; blank lines are not items (that is what a trailing newline would
      // otherwise become).
      return { value: (typeof draft === 'string' ? draft : '').split('\n').map((s) => s.trim()).filter((s) => s !== '') }
    case 'int':
      if (!/^-?\d+$/.test(text)) return { error: text === '' ? '不能为空' : `不是整数：${text}` }
      return { value: Number(text) }
    case 'float': {
      const n = Number(text)
      if (text === '' || !Number.isFinite(n)) return { error: text === '' ? '不能为空' : `不是数字：${text}` }
      return { value: n }
    }
    default:
      // Includes the durations: a duration leaf is a string here ("30s" is what the file holds
      // and what the backend parses), so it needs no case of its own.
      return { value: typeof draft === 'string' ? draft : '' }
  }
}

/** readonlyText is what a field with no input shows. `other` is the kind that has no single
 *  scalar to edit: a map (`provider_aliases`), or a value that is UNSET because unset is
 *  itself meaningful (`vision: nil` = detect it from the model, `max_retries: nil` = the
 *  provider's own default). A box would have to invent a way to spell those, and a checkbox
 *  would assert something the config does not say. */
function readonlyText(f: SettingFieldVO): string {
  if (f.value === null || f.value === undefined) return '未设置（= 自动）'
  try {
    return JSON.stringify(f.value)
  } catch {
    return String(f.value)
  }
}

// ── Config lists ───────────────────────────────────────────────────────────────
//
// `providers.0.api_key`, `providers.1.base_url`, … is how a PATH addresses one item of a config
// list, and as a form label it is unreadable: the reader is looking at "the second provider",
// not at index 1 of a key. So the leaves of one item are drawn as ONE card whose header names
// the item, and whose body holds the item's own leaves.
//
// Both rules below are about the SHAPE of a path, not about any config key: which segment is a
// list index, and which leaf names identify an item. Nothing here knows what `providers` is.

/** The leaf names that identify an item of a config list, in the order they are shown.
 *
 *  `name` where the item has one (`providers.N`, `lsp.servers.N`), `type` where it does not
 *  (`web_search.providers.N` is a backend kind rather than a named thing), and `model` as the
 *  second half of the label — it is not an identity on its own, but "main" alone would not say
 *  which model it points at, and that pair is how a reader tells two providers apart.
 *
 *  These are the config's own vocabulary (the same words its yaml uses), which is why they can
 *  be constants here: an item with none of them falls back to its path, and says so by looking
 *  like a path. */
const ELEMENT_NAME_LEAVES = ['name', 'type'] as const
const ELEMENT_DETAIL_LEAVES = ['model'] as const

/** How many leaves an item may have before its card starts folded. Small items (a hook command
 *  is a command and a timeout) cost more clicks folded than they save scrolling; a provider is
 *  eight leaves and buries the list.
 *
 *  This is the DEFAULT, and the backend can override it for a whole list: a `web_search` /
 *  `web_fetch` provider is three leaves but the largest thing on its page, so those items are
 *  declared folded (SettingListVO.folded) whatever their size. Size alone cannot say that — see
 *  settingFoldedLists in desktop/settings.go. */
const ELEMENT_FOLD_OVER = 3

/** The lists whose items are declared folded (SettingListVO.folded), by the list's own path. An
 *  item's path is its list's path plus an index, so one lookup per item answers it. */
function foldedLists(schema: SettingsSchemaVO | null): Set<string> {
  return new Set((schema?.lists ?? []).filter((l) => l.folded).map((l) => l.path))
}

/** The list a card belongs to: its path minus the trailing index. null when the path is not an
 *  item of a list, which is the same rule elementOf uses for the boundary. */
function listOf(elementPath: string): string | null {
  const at = elementPath.lastIndexOf('.')
  if (at <= 0) return null
  return /^\d+$/.test(elementPath.slice(at + 1)) ? elementPath.slice(0, at) : null
}

/** elementOf splits a leaf path into the list item that owns it and the leaf's own tail:
 *  `providers.0.spec.timeout` → `providers.0` + `spec.timeout`. null = the path is not inside a
 *  list, and the leaf keeps a row of its own.
 *
 *  Which paths count is decided by the SCHEMA, not by the path alone (`listPaths`), and the reason is
 *  the staged item: the page's ＋ stages an item that is not in the config, and the schema describes
 *  its leaves from the element's TYPE (see stagedFields in desktop/settings.go) — so they arrive as
 *  ordinary fields under a path (`providers.1.name`) that no walk of the config would produce. They
 *  are still one item, and must still fold into one card, so the set of paths that ARE items comes
 *  from the backend and a numeric segment alone is not enough.
 *
 *  The boundary is the RIGHTMOST segment that names a list — `hooks.events.pre_tool.0.command` is the
 *  first command of the pre_tool list, not something nested inside a `0` field of a
 *  `hooks.events.pre_tool` item. */
function elementOf(key: string, listPaths: Set<string>): { path: string; tail: string } | null {
  const segs = key.split('.')
  for (let i = segs.length - 1; i >= 0; i--) {
    if (/^\d+$/.test(segs[i]) && listPaths.has(segs.slice(0, i).join('.'))) {
      return { path: segs.slice(0, i + 1).join('.'), tail: segs.slice(i + 1).join('.') }
    }
  }
  return null
}

/** The config lists the page knows about, by their own path (`providers`, `web_search.providers`).
 *  One set, built from the backend's declaration, for every "is this an item of a list" question —
 *  a list the backend does not declare is not one this page may draw as a card. */
function listPathsOf(schema: SettingsSchemaVO | null): Set<string> {
  return new Set((schema?.lists ?? []).map((l) => l.path))
}

/** elementLocator is the card's subtitle: WHICH list item this is.
 *
 *  Two things it does that the raw path does not:
 *    · the index is written `[0]` instead of `.0` — `providers[0]` says "the first item of a
 *      list", where `providers.0` reads as one more level of nesting (the number stays the
 *      config's own: 0-based, the same index config.yaml holds);
 *    · the block heading's prefix is dropped, because the heading already said it — but the
 *      segment that NAMES the list is never dropped: `[0]` on its own would be an index into
 *      nothing. So `web_fetch.providers.0` under a `web_fetch` heading reads `providers[0]`,
 *      while `providers.0` under a `providers` heading stays `providers[0]`.
 *
 *  A path with no index in it is not a list item at all and comes back unchanged. */
function elementLocator(path: string, prefix: string): string {
  const segs = path.split('.')
  let kept = segs
  if (prefix !== '' && path.startsWith(prefix + '.')) {
    kept = segs.slice(prefix.split('.').length)
    if (kept.length < 2) kept = segs.slice(-2)
  }
  const last = kept.length - 1
  return /^\d+$/.test(kept[last]) ? `${kept.slice(0, last).join('.')}[${kept[last]}]` : kept.join('.')
}

/** shorten drops a block's prefix from a path: `web_fetch.proxy` → `proxy`, because the heading
 *  above it already said which block the row belongs to. */
function shorten(key: string, prefix: string): string {
  return prefix !== '' && key.startsWith(prefix + '.') ? key.slice(prefix.length + 1) : key
}

/** elementLabel is an item's header: what it is called, plus the one leaf that says which one of
 *  its kind it is. Falls back to the item's own locator — an item with no name has nothing else
 *  to be called.
 *
 *  `valueOf` is the CURRENT value of a leaf, which is not the same as the one that was read: a
 *  reader renaming a provider must see the header change while they type it, or the card goes on
 *  calling itself by the name they just replaced. */
function elementLabel(
  path: string, fields: SettingFieldVO[], valueOf: (f: SettingFieldVO) => unknown, prefix: string,
): string {
  const text = (leaf: string) => {
    const f = fields.find((x) => x.key === `${path}.${leaf}`)
    if (!f) return ''
    const v = valueOf(f)
    return typeof v === 'string' ? v : ''
  }
  const name = ELEMENT_NAME_LEAVES.map(text).find((v) => v !== '') ?? ''
  const details = ELEMENT_DETAIL_LEAVES.map(text).filter((v) => v !== '')
  return [name, ...details].filter((v) => v !== '').join(' · ') || elementLocator(path, prefix)
}

/** What one group renders as, in the order the config declares it. */
type Item =
  | { kind: 'row'; field: SettingFieldVO }
  | { kind: 'elem'; path: string; label: string; locator: string; fields: SettingFieldVO[] }

/** itemsOf folds a group's paths into cards. The fields arrive in path order, so one item's
 *  leaves are already adjacent and the fold is a single walk — no re-sorting, which would move
 *  a leaf away from the position the config declares it in. */
function itemsOf(fields: SettingFieldVO[], valueOf: (f: SettingFieldVO) => unknown, prefix: string,
                 listPaths: Set<string>): Item[] {
  const items: Item[] = []
  let path = ''
  let bucket: SettingFieldVO[] = []
  const flush = () => {
    if (path !== '') {
      items.push({
        kind: 'elem', path, locator: elementLocator(path, prefix),
        label: elementLabel(path, bucket, valueOf, prefix), fields: bucket,
      })
    }
    path = ''
    bucket = []
  }
  for (const f of fields) {
    const el = elementOf(f.key, listPaths)
    if (!el) {
      flush()
      items.push({ kind: 'row', field: f })
      continue
    }
    if (el.path !== path) {
      flush()
      path = el.path
    }
    bucket.push(f)
  }
  flush()
  return items
}

/** commonPrefix is the longest dot-segment prefix every key shares: `debug.pprof` for
 *  `debug.pprof.enabled` + `debug.pprof.port`, `web_search` for a block that also holds its own
 *  scalars (`web_search.proxy` next to `web_search.providers.0.*`).
 *
 *  It stops one segment short of the shortest key, so a heading can never repeat a whole key —
 *  a heading says which block the rows below belong to, and a key is not a block. */
function commonPrefix(keys: string[]): string {
  const first = keys[0].split('.')
  let n = first.length - 1
  for (const k of keys.slice(1)) {
    const segs = k.split('.')
    let i = 0
    while (i < n && i < segs.length && segs[i] === first[i]) i++
    n = Math.min(n, i)
  }
  return first.slice(0, n).join('.')
}

/** A block of one group: the items that share a top-level config key.
 *
 *  The form used to be ONE flat list per category, so `web_search.*` and `web_fetch.*` ran into
 *  each other and the only thing saying where one block ended was the reader's eye on the path
 *  prefixes. A heading costs a line and buys the whole structure.
 *
 *  prefix is '' when there is nothing to head: a block that holds a single item would be a
 *  heading over one row, which is a line of noise. Those items are drawn bare, with their full
 *  keys — a key with no block above it has to say where it belongs itself. */
type Section = {
  key: string
  prefix: string
  items: Item[]
  /** The declared LIST this block can grow, when it holds one. The backend declares them
   *  (SettingListVO) because an empty list has no fields — without the declaration the page
   *  could not offer the first item of a list, which is exactly when someone wants it. */
  list?: string
}

/** sectionsOf groups one category into blocks, in the config's own path order (a string sort
 *  already keeps one top-level key's leaves together, so this is a single walk), and attaches
 *  each declared list to the block that draws it (or gives it a block of its own when it has no
 *  fields at all). */
function sectionsOf(
  fields: SettingFieldVO[], valueOf: (f: SettingFieldVO) => unknown, lists: string[], listPaths: Set<string>,
): Section[] {
  const out: Section[] = []
  let seg = ''
  let bucket: SettingFieldVO[] = []
  const flush = () => {
    if (seg === '') return
    const prefix = bucket.length >= 2 ? commonPrefix(bucket.map((f) => f.key)) : ''
    out.push({ key: bucket[0].key, prefix, items: itemsOf(bucket, valueOf, prefix, listPaths) })
    seg = ''
    bucket = []
  }
  for (const f of fields) {
    const s = f.key.split('.')[0]
    if (s !== seg) {
      flush()
      seg = s
    }
    bucket.push(f)
  }
  flush()

  for (const list of lists) {
    // The innermost block whose prefix the list lives under — `web_search.providers` belongs to
    // the `web_search` block, and `providers` to a `providers` one.
    const host = out.filter((sec) => sec.prefix !== '' && (list === sec.prefix || list.startsWith(sec.prefix + '.')))
      .sort((a, b) => b.prefix.length - a.prefix.length)[0]
    if (host) host.list = list
    else out.push({ key: list, prefix: list, items: [], list })
  }
  return out
}

export function SettingsPage({ onBack }: { onBack: () => void }) {
  const [schema, setSchema] = useState<SettingsSchemaVO | null>(null)
  const [loadError, setLoadError] = useState('')
  const [drafts, setDrafts] = useState<Record<string, Draft>>({})
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [active, setActive] = useState('')
  const [notice, setNotice] = useState<{ bad: boolean; text: string } | null>(null)
  const [saving, setSaving] = useState(false)
  // An add or a remove in flight. They are not "saves" (they write immediately, and a save's
  // draft rules do not apply), so they get their own flag — and the same notice line.
  const [busy, setBusy] = useState(false)
  // The item that was just added, so its card opens: a new element is 11 leaves of nothing, and
  // the reason the reader pressed ＋ was to fill them in.
  const [justAdded, setJustAdded] = useState('')

  // load re-reads the whole schema. It is also what a SUCCESSFUL save ends with: the values in
  // the file are the truth, and the form is not — so the page takes the new values (and drops
  // the drafts) instead of assuming its own edits landed as typed.
  //
  // The notice is deliberately NOT cleared here: the save's summary is set after this returns,
  // and a reload that wiped it would swallow the one sentence that says what happened.
  const load = useCallback(async () => {
    const res = await AgentService.GetSettingsSchema().catch((e) => {
      setLoadError(String(e))
      return null
    })
    if (!res) return
    setLoadError('')
    setSchema(res)
    setDrafts({})
    setFieldErrors({})
  }, [])

  useEffect(() => { void load() }, [load])

  const fields = useMemo(() => schema?.fields ?? [], [schema])

  // What each field was when it was read: the yardstick for "dirty", and what a box is reset to.
  const original = useMemo(() => {
    const m = new Map<string, Draft>()
    for (const f of fields) m.set(f.key, fieldDraft(f))
    return m
  }, [fields])

  const byKey = useMemo(() => {
    const m = new Map<string, SettingFieldVO>()
    for (const f of fields) m.set(f.key, f)
    return m
  }, [fields])

  const dirty = useMemo(
    () => Object.keys(drafts).filter((k) => drafts[k] !== original.get(k)),
    [drafts, original],
  )

  // The nav IS the backend's list, in its order. A group with no field is skipped rather than
  // drawn empty: the guard test says that cannot happen for the table, but a category whose
  // leaves are all dynamic could still be empty for one config, and a nav entry that opens onto
  // nothing is worse than one that is not there.
  const groups = useMemo(() => {
    const byCat = new Map<string, SettingFieldVO[]>()
    for (const f of fields) {
      const list = byCat.get(f.category)
      if (list) list.push(f)
      else byCat.set(f.category, [f])
    }
    return (schema?.categories ?? [])
      .map((name) => ({ name, fields: byCat.get(name) ?? [] }))
      .filter((g) => g.fields.length > 0)
  }, [schema, fields])

  // The group on screen. The first one until the reader picks, and never a group this schema
  // does not have (a config can change under the page: the save reloads it).
  const shown = groups.some((g) => g.name === active) ? active : (groups[0]?.name ?? '')
  const shownFields = groups.find((g) => g.name === shown)?.fields ?? []
  // The lists this group may grow. Declared by the backend rather than inferred from the cards,
  // because an empty list has no cards — and an empty list is exactly where ＋ matters most.
  const listsHere = (schema?.lists ?? []).filter((l) => l.category === shown).map((l) => l.path)
  // The lists whose items open FOLDED whatever their size (see ELEMENT_FOLD_OVER): a schema
  // declaration, not a size the page measures.
  const foldedHere = foldedLists(schema)

  // Going back only. The page has no state worth keeping across a return: the schema is
  // re-read on the way in, which is what makes the values on screen the values in the file.
  const back = useCallback(() => onBack(), [onBack])

  // Esc returns to the conversation, the keyboard's way out of a page whose only other exit is
  // the rail's 「返回对话」 — the same gesture every other surface here has (a viewer, the side
  // panel, a palette).
  //
  // It CLAIMS the key (capture + stopPropagation) rather than merely acting on it: the chat view
  // stays mounted behind this page, so its own Esc listeners are alive, and an unclaimed Escape
  // would also close whatever panel was left open under here — a change the reader cannot see
  // and did not ask for, on a page whose contract is "nothing outside it moves". The unlock
  // confirm, a select's own popup and an IME candidate window take the key before we do, which is
  // correct: those are the reader's current context.
  //
  // A dirty form is NOT a reason to stay or to ask: this returns exactly what 「返回对话」 does
  // today, and a page whose two exits disagree is a page the reader has to remember. The drafts
  // are dropped either way, and the foot's 「N 项未保存」 is what said so before they pressed it.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented) return
      // An Escape the IME is using to dismiss its candidate window is the reader's current
      // context (and it fires before the composition is torn down), not a way out of the page.
      if (e.isComposing) return
      e.preventDefault()
      e.stopPropagation()
      back()
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [back])

  // addItem grows a list. The item is STAGED, not written (see AddSettingsItem): it becomes part of
  // the next save, and the page re-reads so the card the backend names is drawn — with its inputs,
  // which the backend describes from the element's type precisely because the element is not in the
  // config yet. The card opens: the reason someone presses ＋ is to fill the new item in.
  //
  // The re-read is a round trip for a change the page made itself, but it is the right one here: the
  // schema is the only thing that knows what the new item's leaves ARE (a staged element has no
  // values to walk), so drawing it from anything but the backend would be the page inventing them.
  const addItem = useCallback(async (listPath: string) => {
    setBusy(true)
    const res = await AgentService.AddSettingsItem(listPath).catch((e) => {
      setNotice({ bad: true, text: String(e) })
      return null
    })
    setBusy(false)
    if (!res) return
    setNotice({ bad: res.ok !== true, text: res.summary || '' })
    if (res.ok && res.elementPath) setJustAdded(res.elementPath)
    await load()
  }, [load])

  // removeItem marks an item for removal (or cancels one that is still staged — the backend tells
  // the two apart). Nothing is written, so the page re-reads only to pick up the notice: the card
  // stays where it is until the save, which is what 「点保存后才生效」 means.
  const removeItem = useCallback(async (elementPath: string) => {
    setBusy(true)
    const res = await AgentService.RemoveSettingsItem(elementPath).catch((e) => {
      setNotice({ bad: true, text: String(e) })
      return null
    })
    setBusy(false)
    if (!res) return
    setNotice({ bad: res.ok !== true, text: res.summary || '' })
    setJustAdded('')
  }, [])

  const save = useCallback(async () => {
    const updates: Record<string, any> = {}
    const errors: Record<string, string> = {}
    for (const key of dirty) {
      const f = byKey.get(key)
      if (!f) continue
      const parsed = parseField(f, drafts[key])
      if (parsed.error) errors[key] = parsed.error
      else updates[key] = parsed.value
    }
    setFieldErrors(errors)
    const bad = Object.keys(errors)
    if (bad.length > 0) {
      setNotice({ bad: true, text: `有 ${bad.length} 项填得不对，没有保存：${bad.join('、')}` })
      return
    }
    if (dirty.length === 0) {
      setNotice({ bad: false, text: '没有改动需要保存' })
      return
    }

    setSaving(true)
    const res = await AgentService.SaveSettings(updates).catch((e) => {
      setNotice({ bad: true, text: String(e) })
      return null
    })
    setSaving(false)
    if (!res) return
    // A save that wrote re-reads (the values are the new ones). A save that was REFUSED does
    // not: nothing changed anywhere, and the reader's typing has to survive — they are about to
    // fix whatever the sentence named. Either way the sentence is shown as the backend wrote it
    // (this page does not translate Go errors).
    if (res.ok) await load()
    setNotice({ bad: res.ok !== true, text: res.summary || '' })
  }, [dirty, byKey, drafts, load])

  // rowProps is every row's wiring, in one place: a standalone row and a row inside a card must
  // behave identically (same dirty rule, same draft, same reset), and the only thing that
  // differs is the label a card gives its leaves (the tail of the path, since the card already
  // says which item they belong to).
  //
  // Dirty is "a draft exists and differs from what was read": typing a value back to what it was
  // is NOT a change, and a row that only LOOKS different would otherwise be saved as one.
  const isDirty = (f: SettingFieldVO) => drafts[f.key] !== undefined && drafts[f.key] !== original.get(f.key)
  /** What a leaf holds RIGHT NOW: the draft when there is one, the value that was read otherwise.
   *  A card's header is built from this, so it follows a rename as it is typed. */
  const currentValue = (f: SettingFieldVO) => (drafts[f.key] !== undefined ? drafts[f.key] : f.value)
  const rowProps = (f: SettingFieldVO) => ({
    key: f.key,
    f,
    draft: drafts[f.key] as Draft | undefined,
    original: original.get(f.key),
    error: fieldErrors[f.key],
    dirty: isDirty(f),
    onChange: (v: Draft) => setDrafts((d) => ({ ...d, [f.key]: v })),
    onReset: () => setDrafts((d) => {
      const next = { ...d }
      delete next[f.key]
      return next
    }),
  })

  return (
    <div className="settings-view">
      <div className="settings-rail">
        <button className="settings-back" onClick={back} title="返回对话（设置已保存的部分不受影响）">
          <span aria-hidden="true">←</span> 返回对话
        </button>
        <div className="settings-group-label">设置</div>
        {groups.map((g) => (
          <button key={g.name} className={`settings-cat${g.name === shown ? ' is-active' : ''}`}
            onClick={() => setActive(g.name)} aria-current={g.name === shown ? 'true' : undefined}>
            <span>{g.name}</span>
            <span className="settings-cat-count">{g.fields.length}</span>
          </button>
        ))}
      </div>

      <div className="settings-pane">
        <div className="settings-head">
          <h2 className="settings-title">{shown || '设置'}</h2>
          <div className="settings-sub">
            {loadError
              ? '读不到配置'
              : `${shownFields.length} 个可配置项 · 值的形态决定了输入框：开关、文本框、或每行一项的列表`}
          </div>
        </div>

        <div className="settings-form">
          {loadError ? (
            <div className="settings-error">读不到配置：{loadError}</div>
          ) : !schema ? (
            <div className="settings-empty">正在读取配置…</div>
          ) : shownFields.length === 0 ? (
            <div className="settings-empty">这一类下没有可配置项。</div>
          ) : (
            sectionsOf(shownFields, currentValue, listsHere, listPathsOf(schema)).map((sec) => {
              // A row inside a block is labelled by the REST of its path: the heading already said
              // which block this is, and repeating it on every row is the noise the heading was
              // added to remove. (FieldRow keeps the full key in the tooltip.)
              const items = sec.items.map((it) =>
                it.kind === 'row' ? (
                  <FieldRow {...rowProps(it.field)} label={shorten(it.field.key, sec.prefix)} />
                ) : (
                  <ElementCard key={it.path} item={it}
                    dirtyCount={it.fields.filter(isDirty).length}
                    hasError={it.fields.some((f) => fieldErrors[f.key])}
                    forceOpen={it.path === justAdded}
                    forceFolded={!!listOf(it.path) && foldedHere.has(listOf(it.path)!)}
                    onRemove={sec.list ? () => void removeItem(it.path) : undefined}
                    renderRow={(f, label) => <FieldRow {...rowProps(f)} label={label} />} />
                ),
              )
              // No heading: the item is on its own, and its full key is what says where it
              // belongs. A fragment, because the row/card keys are its own.
              return sec.prefix === ''
                ? <Fragment key={sec.key}>{items}</Fragment>
                : (
                  <div className="set-section" key={sec.key}>
                    <div className="set-section-head">{sec.prefix}</div>
                    {items}
                    {sec.list ? (
                      // The button NAMES its list (`providers`, not just "一项"): a block can hold a
                      // list and scalars at once, and `web_search`'s ＋ adds a web_search PROVIDER,
                      // which the button has to say or the reader cannot tell what they are making.
                      <div className="set-add">
                        {sec.items.length === 0
                          ? <span className="set-add-empty">{shorten(sec.list, sec.prefix)} 还没有条目</span>
                          : null}
                        <button className="btn" disabled={busy} onClick={() => void addItem(sec.list!)}>
                          ＋ 添加 {shorten(sec.list, sec.prefix)}
                        </button>
                      </div>
                    ) : null}
                  </div>
                )
            })
          )}
        </div>

        <div className="settings-foot">
          <div className={`settings-notice${notice?.bad ? ' is-bad' : ''}`}>{notice?.text ?? ''}</div>
          {dirty.length > 0 ? <div className="settings-dirty-count">{dirty.length} 项未保存</div> : null}
          <button className="btn" onClick={() => void save()} disabled={saving || dirty.length === 0}>
            {saving ? '保存中…' : '保存'}
          </button>
        </div>
      </div>
    </div>
  )
}

/** One row: the key it edits, why it behaves the way it does, and the input its Kind asks for.
 *  `label` is what to SHOW as the key — inside an element card it is the tail of the path, since
 *  the card's header already names the item; the full path stays in the tooltip either way, so a
 *  reader editing config.yaml by hand is never more than a hover away from it. */
function FieldRow({ f, label, draft, original, error, dirty, onChange, onReset }: {
  f: SettingFieldVO
  label?: string
  draft: Draft | undefined
  original: Draft | undefined
  error: string | undefined
  dirty: boolean
  onChange: (v: Draft) => void
  onReset: () => void
}) {
  const value = draft !== undefined ? draft : (original ?? '')
  const className = `set-row${dirty ? ' is-dirty' : ''}${error ? ' is-invalid' : ''}`
  const shownKey = label ?? f.key
  return (
    <div className={className}>
      <div>
        <div className="set-key" title={shownKey === f.key ? undefined : f.key}>
          {shownKey}
          {/* The badge is the backend's derived answer (settingApply.needsRestart), so it cannot
              disagree with what a save will actually do. */}
          {f.needsRestart ? <span className="set-restart" title="保存后要重启 Tachi 才生效">⟳ 需重启</span> : null}
        </div>
        {f.note ? <div className="set-note">{f.note}</div> : null}
      </div>
      <div>
        {control(f, value, onChange)}
        {f.kind === 'string_list' ? <div className="set-list-hint">每行一项</div> : null}
        {error ? <div className="set-field-error">{error}</div> : null}
        {/* Only while the row is changed: a "撤销这一项" on every row would be noise on a page
            whose normal state is "nothing changed". */}
        {dirty ? (
          <button className="btn ghost" style={{ marginTop: 6, padding: '2px 8px', fontSize: 11.5 }}
            onClick={onReset} title="把这一项改回打开这一页时的值（不写文件）">撤销这一项</button>
        ) : null}
      </div>
    </div>
  )
}

/** ElementCard is one item of a config list: a header that says WHICH item it is, and its own
 *  leaves inside. The path (`providers.0`) is demoted to a dimmed subtitle rather than being the
 *  label — it is still what a reader needs in order to find the block in config.yaml, but it is
 *  not what they are looking at.
 *
 *  Folded by default when the item is big, or when its whole LIST is declared folded (a
 *  web_search provider is three leaves and still the largest thing on its page — see
 *  settingFoldedLists in desktop/settings.go). Small items in an ordinary list stay open, where a
 *  fold would cost a click and save nothing. An item holding a value the form REFUSED is forced
 *  open: an error behind a fold is an error the reader cannot act on. */
function ElementCard({ item, dirtyCount, hasError, forceOpen, forceFolded, onRemove, renderRow }: {
  item: Extract<Item, { kind: 'elem' }>
  dirtyCount: number
  hasError: boolean
  /** Open this card whatever the default is: an item that was just ADDED is where the reader is
   *  about to type, and eleven empty leaves behind a fold is a card they would have to find. It
   *  outranks forceFolded — filling in a new item is exactly what ＋ promised. */
  forceOpen?: boolean
  /** The item's list is declared folded (SettingListVO.folded): shut whatever its size. */
  forceFolded?: boolean
  /** Removing is only offered for an item of a declared list (see Section.list). It is a two-step
   *  button rather than one: the write is immediate and there is no undo, so the second click is
   *  the confirmation. */
  onRemove?: () => void
  renderRow: (f: SettingFieldVO, label: string) => ReactNode
}) {
  // null = follow the default, so an item that grows or shrinks still opens the way a first
  // visit would; a click pins it either way.
  const [open, setOpen] = useState<boolean | null>(null)
  const [confirmRemove, setConfirmRemove] = useState(false)
  // Folded by default when the list says so, or when the item is big (a provider is eleven
  // leaves and a page of them buries the list) — or when the reader pinned it that way. An item
  // that was just added opens: the reason someone pressed ＋ is to fill it in.
  const defaultFolded = !forceOpen && (forceFolded || item.fields.length > ELEMENT_FOLD_OVER)
  const folded = hasError ? false : (open ?? defaultFolded)
  return (
    <div className="set-elem">
      <button type="button" className="set-elem-head" onClick={() => setOpen(!folded)} aria-expanded={!folded}>
        <span className="set-elem-caret" aria-hidden="true"><Caret open={!folded} /></span>
        <span className="set-elem-name">{item.label}</span>
        {item.label === item.locator ? null : <span className="set-elem-path">{item.locator}</span>}
        <span className="set-elem-flags">
          {hasError
            ? <span className="set-elem-bad">有填错</span>
            : dirtyCount > 0 ? <span className="set-elem-count">{dirtyCount} 项未保存</span> : null}
        </span>
      </button>
      {folded ? null : (
        <div className="set-elem-body">
          {item.fields.map((f) => renderRow(f, f.key.slice(item.path.length + 1)))}
          {onRemove ? (
            confirmRemove ? (
              <div className="set-elem-confirm">
                <span>删除 {item.locator}？会立刻写进 config.yaml。</span>
                <button className="btn danger" onClick={onRemove}>删除</button>
                <button className="btn ghost" onClick={() => setConfirmRemove(false)}>取消</button>
              </div>
            ) : (
              // 危险色：这是唯一会把东西从用户配置里拿走的入口，和旁边的「撤销这一项」（中性灰）
              // 必须一眼分得开。`.ghost` 只去掉边框，颜色留在 `.danger` 上。
              <button className="btn danger ghost" onClick={() => setConfirmRemove(true)}>删除这一项…</button>
            )
          ) : null}
        </div>
      )}
    </div>
  )
}

/** control is the one place a Kind becomes a widget. Every branch is a Kind the backend can
 *  actually produce (kindOf) — a new Kind wants a branch here, and `other` is the fallback that
 *  still shows the value rather than dropping the field. */
function control(f: SettingFieldVO, value: Draft, onChange: (v: Draft) => void) {
  // An OPTIONAL leaf gets the 自动 state first, because it is a state of its own and every control
  // below would have to invent a way to show it: a checkbox would say 关 for a value that is
  // neither true nor false, and an empty box would look like something nobody finished typing.
  if (f.optional) return autoControl(f, value, onChange)
  return plainControl(f, value, onChange)
}

/** autoControl is an optional leaf's control: 自动 / a value.
 *
 *  A switch gets a three-entry list instead of a switch — there is no third position on a
 *  checkbox, and "自动 + a checkbox" is two controls for one value. Everything else keeps its own
 *  control, with 自动 beside it and the control hidden while 自动 is on: an empty number box next
 *  to 自动 reads as "unfinished", which is the impression this is avoiding. */
function autoControl(f: SettingFieldVO, value: Draft, onChange: (v: Draft) => void) {
  if (f.kind === 'bool') {
    return (
      <div>
        <select className="set-input set-select" value={value === null ? '' : value === true ? 'true' : 'false'}
          onChange={(e) => onChange(e.target.value === '' ? null : e.target.value === 'true')}>
          <option value="">自动（未设置）</option>
          <option value="true">开</option>
          <option value="false">关</option>
        </select>
        {value === null ? <div className="set-note">由 Tachi 自己决定</div> : null}
      </div>
    )
  }
  return (
    <div>
      <label className="set-check">
        <input type="checkbox" checked={value === null}
          onChange={(e) => onChange(e.target.checked ? null : '')} />
        <span>自动（未设置）</span>
      </label>
      {value === null
        ? <div className="set-note">由 Tachi 自己决定</div>
        : plainControl(f, value, onChange)}
    </div>
  )
}

function plainControl(f: SettingFieldVO, value: Draft, onChange: (v: Draft) => void) {
  const text = typeof value === 'string' ? value : ''
  switch (f.kind) {
    case 'bool':
      return (
        <label className="set-check">
          <input type="checkbox" checked={value === true} onChange={(e) => onChange(e.target.checked)} />
          <span>{value === true ? '开' : '关'}</span>
        </label>
      )
    case 'string_list':
      return (
        <textarea className="set-textarea" rows={3} value={text} spellCheck={false}
          onChange={(e) => onChange(e.target.value)}
          placeholder="每行一项，例如：&#10;git *&#10;npm *" />
      )
    case 'int':
      return <input className="set-input" type="text" inputMode="numeric" value={text} spellCheck={false}
        onChange={(e) => onChange(e.target.value)} />
    case 'float':
      return <input className="set-input" type="text" inputMode="decimal" value={text} spellCheck={false}
        onChange={(e) => onChange(e.target.value)} />
    case 'string': {
      // A declared choice set becomes a dropdown (the backend's `options`, which is what THIS
      // config defines — the provider names, today). Two details are about not lying:
      //
      //   • the current value is always IN the list, even when the config does not define it (a
      //     provider that was renamed or removed). A `<select>` whose value is absent from its
      //     options silently shows the first one instead, and the next save would then persist a
      //     change nobody made.
      //   • the empty value gets its own entry: every key with choices today means "fall back to
      //     the default" when empty, and there would otherwise be no way back to it once a value
      //     has been picked.
      const options = f.options ?? []
      if (options.length === 0) {
        return <input className="set-input" type="text" value={text} spellCheck={false}
          onChange={(e) => onChange(e.target.value)} />
      }
      return (
        <select className="set-input set-select" value={text} onChange={(e) => onChange(e.target.value)}>
          <option value="">（默认）</option>
          {options.map((o) => <option key={o} value={o}>{o}</option>)}
          {text !== '' && !options.includes(text)
            ? <option value={text}>{text}（当前值，不在可选列表里）</option>
            : null}
        </select>
      )
    }
    default:
      return (
        <div>
          <div className="set-readonly">{readonlyText(f)}</div>
          <div className="set-note">
            {f.value === null || f.value === undefined
              ? '未设置 = 由 Tachi 自动决定，界面上改不了这一项，请直接编辑 config.yaml'
              : '这一项不是单个值（列表或映射），界面上改不了，请直接编辑 config.yaml'}
          </div>
        </div>
      )
  }
}
