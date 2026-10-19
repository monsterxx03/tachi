import { Fragment, useCallback, useEffect, useMemo, useState, type ReactNode } from 'react'
import { AgentService } from '../bindings/github.com/monsterxx03/tachi/desktop'
import type { SettingFieldVO, SettingListVO, SettingsPreviewVO, SettingsSchemaVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { Caret, EyeIcon } from './components'
import { DiffLines } from './diff'
import { MCPServersEditor } from './mcpeditor'
import { MobilePairingCard } from './mobilepairing'
import { IS_MAC } from './platform'

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
// refused save does to the reader's typing — and the items it is holding that the config is not (an
// item ＋ made, an item 删除 dropped). All of it is unsaved state and all of it goes to the same
// save: a page owns what it has not written yet, and nothing else does.

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
//
// ADDING and REMOVING live here too, and they are THIS PAGE's state — the same kind of thing as a
// draft. An item the reader adds exists nowhere but on this screen until the save that carries it
// (see SaveSettings): that is what lets them press ＋, look at the card, and walk away with nothing
// written. So the backend hands over what an item of a list LOOKS like (`SettingListVO`) and the
// page keeps the rest — which item it is holding, and which one it dropped.

/** The leaf names that identify an item of a config list, in the order they are shown.
 *
 *  `name` where the item has one (`providers.N`), `type` where it does not
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
 *  the item the page is holding: ＋ makes a card for an item that is not in the config, whose leaves
 *  arrived from the list's TEMPLATE (SettingListVO.element) — ordinary fields under a path
 *  (`providers.1.name`) that no walk of the config would produce. They are still one item, and must
 *  still fold into one card, so the set of paths that ARE items comes from the backend and a numeric
 *  segment alone is not enough.
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

/** listByPath resolves a list's own path to the backend's declaration of it: how many items it holds
 *  now (`length`) and what one of its items looks like (`element`) — the two facts this page cannot
 *  read off a value, because the item it is about to draw does not exist. */
function listByPath(schema: SettingsSchemaVO | null, path: string): SettingListVO | undefined {
  return (schema?.lists ?? []).find((l) => l.path === path)
}

/** One item the page is holding and the config is not: the path it will have (`providers.3`) and
 *  the list it belongs to. */
type PendingAdd = { path: string; list: string }

/** pendingFields draws the leaves of the items the page has added: ordinary rows, synthesized from
 *  the list's TEMPLATE — the same shape the backend sends for a real item, so the card they make is
 *  edited, validated and saved by the same machinery as every other row.
 *
 *  The KEY is the one thing the template cannot carry: the template is the same for every item of a
 *  list, and the path is per item (`providers.3.name`). Composing it here is what makes the item the
 *  page's own: `providers.3` is the index the page chose (the list's length plus the items it is
 *  already holding), and the save checks that the config agrees. */
function pendingFields(schema: SettingsSchemaVO | null, adds: PendingAdd[]): SettingFieldVO[] {
  const out: SettingFieldVO[] = []
  for (const add of adds) {
    for (const leaf of listByPath(schema, add.list)?.element ?? []) {
      out.push({
        key: `${add.path}.${leaf.tail}`,
        kind: leaf.kind,
        value: leaf.value,
        options: leaf.options,
        optional: leaf.optional,
        isSet: leaf.isSet,
        needsRestart: leaf.needsRestart,
        category: leaf.category,
        note: leaf.note,
        // Masking travels with the template like everything else: a key typed into a card ＋ made
        // is the same key as one read from the file (see SecretInput).
        secret: leaf.secret,
      })
    }
  }
  return out
}

/** fieldsInOrder merges what the config holds with what the page is holding, in the one order both
 *  sides speak: by path.
 *
 *  The backend hands its own over sorted (config.PathsFor sorts) and the folds below — one item's
 *  leaves into one card, one top-level key into one block — are a SINGLE walk that relies on it, so
 *  a merged list that was not sorted would draw the same block twice (a `providers: 0 …` heading
 *  next to a `providers: 3 …` one) and two cards would stop being one list.
 *
 *  A pending item of `providers` sorts where a real one of the same list does, which is exactly the
 *  point: the reader sees it at the END of the list it is joining, next to where the ＋ that made it
 *  sits. */
function fieldsInOrder(schema: SettingsSchemaVO | null, adds: PendingAdd[]): SettingFieldVO[] {
  return [...(schema?.fields ?? []), ...pendingFields(schema, adds)]
    .sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0))
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
  list?: SettingListVO
}

/** sectionsOf groups one category into blocks, in the config's own path order (a string sort
 *  already keeps one top-level key's leaves together, so this is a single walk), and attaches
 *  each declared list to the block that draws it (or gives it a block of its own when it has no
 *  fields at all).
 *
 *  The heading-less blocks are returned FIRST, ahead of the path order — see the partition at the
 *  end, and the one rule it exists for: a heading covers its own rows and nothing else. */
function sectionsOf(
  fields: SettingFieldVO[], valueOf: (f: SettingFieldVO) => unknown, lists: SettingListVO[], listPaths: Set<string>,
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
    const host = out.filter((sec) => sec.prefix !== '' && (list.path === sec.prefix || list.path.startsWith(sec.prefix + '.')))
      .sort((a, b) => b.prefix.length - a.prefix.length)[0]
    if (host) host.list = list
    else out.push({ key: list.path, prefix: list.path, items: [], list })
  }

  // A heading must cover exactly its own rows. The walk above groups by path, which can leave a
  // one-row block AFTER a headered one — and a bare row sitting under a heading reads as one of
  // its leaves, which is how `max_tokens` came to look like a field of jev: nothing said the block
  // had ended, and the heading was the nearest thing above it. Bare rows go first for that reason;
  // they are the keys with no block to be headed by, and nothing below a heading is anything but
  // its own rows. (They keep their full key — shorten() has no prefix to drop.)
  const bare = out.filter((sec) => sec.prefix === '')
  if (bare.length === 0) return out
  return [...bare, ...out.filter((sec) => sec.prefix !== '')]
}

/** previewTitle is the preview's heading: one sentence that covers whatever the batch would write.
 *  A save can touch config.yaml and one or more provider files (providers live in their own
 *  `provider.*.json` — see desktop/settings.go's SaveSettings), so the title names the FILES that
 *  are actually in play rather than promising "config.yaml". */
function previewTitle(res: SettingsPreviewVO): string {
  const files = res.files || []
  if (files.length === 0) return '保存会写进 config.yaml 的改动'
  const names = files.map((f) => f.name).join('、')
  const hasYAML = (res.hunks || []).length > 0
  return hasYAML ? `保存会写进 config.yaml 与 ${names} 的改动` : `保存会写进 ${names} 的改动`
}

/** pendingText is the foot's count of what the save will carry. The three kinds are one
 *  transaction, so one line has to say all of them — a page holding an item that is not in the
 *  config and reporting "0 项未保存" would be telling the reader nothing is about to happen. */
function pendingText(dirty: number, addCount: number, removeCount: number): string {
  const parts: string[] = []
  if (dirty > 0) parts.push(`${dirty} 项未保存`)
  if (addCount > 0) parts.push(`${addCount} 条待新增`)
  if (removeCount > 0) parts.push(`${removeCount} 条待删除`)
  return parts.join(' · ')
}

export function SettingsPage({ onBack, onSaved }: { onBack: () => void; onSaved?: () => void }) {
  const [schema, setSchema] = useState<SettingsSchemaVO | null>(null)
  const [loadError, setLoadError] = useState('')
  const [drafts, setDrafts] = useState<Record<string, Draft>>({})
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [active, setActive] = useState('')
  const [notice, setNotice] = useState<{ bad: boolean; text: string } | null>(null)
  const [saving, setSaving] = useState(false)
  // A removal being CHECKED against the config (the warning call). It is not a save — the removal
  // is local, and no draft rule applies — so it gets its own flag and shares the notice line.
  const [checking, setChecking] = useState(false)
  // The save's preview: what the file would get, fetched on demand and NEVER applied. `key` is what
  // the batch was when it was taken — the form can move on while the panel is open, and a diff of
  // edits the reader has since changed is the one thing the panel must not pretend about.
  const [preview, setPreview] = useState<{ res: SettingsPreviewVO; key: string } | null>(null)
  const [previewing, setPreviewing] = useState(false)
  // What this page is holding that the config is not, and it is the SAME KIND OF THING as a draft:
  // the items ＋ made (nothing but a path and a set of drafts) and the items 删除 dropped. Both are
  // gone when the page is (the drafts are), and the save is the one place they become real — which
  // is why the backend no longer keeps a copy of either (see SaveSettings).
  const [adds, setAdds] = useState<PendingAdd[]>([])
  const [removes, setRemoves] = useState<string[]>([])

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

  // What the form draws: the config's own leaves AND the leaves of every item this page is holding
  // (pendingFields), in one order — the folds below are a single walk and rely on it.
  const fields = useMemo(() => fieldsInOrder(schema, adds), [schema, adds])

  // What each field was when it was read: the yardstick for "dirty", and what a box is reset to.
  // A pending item's yardstick is the TEMPLATE's value (a new provider starts at its type's
  // defaults), so typing one of them in is a change and leaving it alone is not — which is exactly
  // what "the save carries what the reader typed" means for an item that has nothing yet.
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
  const listsHere = (schema?.lists ?? []).filter((l) => l.category === shown)
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

  // addItem grows a list, entirely on this page: the item becomes a card with inputs (its leaves
  // come from the list's TEMPLATE — see pendingFields), the reader fills it in, and the next save
  // carries both the item and its values in one write.
  //
  // Nothing is asked of the backend and nothing is written, which is the point: ＋ is the one gesture
  // whose whole meaning is "maybe" — press it, look at the card, walk away — and an item that
  // existed nowhere but on this screen is the only kind that can be taken back by leaving.
  //
  // The index is this page's own arithmetic: the list's length (from the schema) plus the items of
  // that list it is ALREADY holding, so pressing ＋ twice yields `providers.3` and `providers.4`
  // rather than naming the same element twice. The save is what holds it to the truth — the element
  // path it is given must be the list's end (config.ListEdits).
  const addItem = useCallback((listPath: string) => {
    setAdds((prev) => {
      const taken = new Set(prev.map((a) => a.path))
      let index = listByPath(schema, listPath)?.length ?? 0
      while (taken.has(`${listPath}.${index}`)) index++
      return [...prev, { path: `${listPath}.${index}`, list: listPath }]
    })
    setNotice(null)
  }, [schema])

  // removeItem drops an item, and it is one gesture: the card leaves the list at once, a line in its
  // place says what was dropped, and the save is what makes it real. There is no second question to
  // ask — nothing is written yet, and 「撤销」 is the answer to a mistake, which a confirmation
  // dialog is a worse version of.
  //
  // What the backend IS asked is the one question this page cannot answer: what still names the item
  // (`SettingsRemovalWarning` — a key like `title_provider` may hold this provider's name, and a
  // warning is only worth having while the reader can still fix it). An item the page is holding and
  // the config never had is dropped without that call: there is nothing to warn about.
  const removeItem = useCallback(async (elementPath: string) => {
    const dropDrafts = () => setDrafts((d) => {
      const next = { ...d }
      for (const key of Object.keys(next)) {
        if (key === elementPath || key.startsWith(elementPath + '.')) delete next[key]
      }
      return next
    })
    if (adds.some((a) => a.path === elementPath)) {
      setAdds((prev) => prev.filter((a) => a.path !== elementPath))
      dropDrafts()
      setNotice(null)
      return
    }
    setChecking(true)
    const res = await AgentService.SettingsRemovalWarning(elementPath).catch((e) => {
      setNotice({ bad: true, text: String(e) })
      return null
    })
    setChecking(false)
    if (!res) return
    if (res.ok !== true) {
      // The page and the config disagree about this list: the card stays where it is, and the
      // sentence says what to do about it.
      setNotice({ bad: true, text: res.summary || '' })
      return
    }
    setRemoves((prev) => (prev.includes(elementPath) ? prev : [...prev, elementPath]))
    dropDrafts()
    // The sentence is this page's, because the removal is: the backend's clause is about the CONFIG
    // (which keys still name the item), and the locator is the page's own vocabulary.
    const marked = `已标记删除 ${elementLocator(elementPath, '')}；保存后才会写进 config.yaml`
    setNotice({ bad: false, text: res.summary ? `${marked}；${res.summary}` : marked })
  }, [adds])

  // undoRemove puts a dropped item back in the list. It is the exact inverse of removeItem: no call
  // to the backend either way, because the removal is nothing but this page's own mark.
  const undoRemove = useCallback((elementPath: string) => {
    setRemoves((prev) => prev.filter((p) => p !== elementPath))
    setNotice({ bad: false, text: `已撤销：${elementLocator(elementPath, '')} 还在 config.yaml 里，什么都没变` })
  }, [])

  // collectBatch is the page's WHOLE unsaved state as the batch the backend takes: the drafts, the
  // items ＋ made (by the path each one was given) and the items 删除 dropped. One function, because
  // the preview and the save have to send the same thing — a preview of a different batch than the
  // save carries would be worse than no preview (the reader acts on it).
  const collectBatch = useCallback(() => {
    const updates: Record<string, any> = {}
    const errors: Record<string, string> = {}
    for (const key of dirty) {
      const f = byKey.get(key)
      if (!f) continue
      const parsed = parseField(f, drafts[key])
      if (parsed.error) errors[key] = parsed.error
      else updates[key] = parsed.value
    }
    return { updates, errors }
  }, [dirty, byKey, drafts])

  // What the batch IS, as a string: the preview remembers it so a form that moved on can say so,
  // instead of showing a diff of edits that are no longer the ones on screen.
  const batchKey = useMemo(
    () => JSON.stringify([dirty.map((k) => [k, drafts[k]]), adds.map((a) => a.path), removes]),
    [dirty, drafts, adds, removes])

  // preview asks the backend what 保存 would write and shows it. It is the SAME batch and the same
  // backend pre-flight, so it answers a second question for free: a batch the save would refuse is
  // refused here, before the reader has committed to anything.
  const openPreview = useCallback(async () => {
    const { updates, errors } = collectBatch()
    setFieldErrors(errors)
    const bad = Object.keys(errors)
    if (bad.length > 0) {
      setNotice({ bad: true, text: `有 ${bad.length} 项填得不对，预览不了：${bad.join('、')}` })
      return
    }
    setPreviewing(true)
    const res = await AgentService.PreviewSettings(updates, adds.map((a) => a.path), removes).catch((e) => {
      setNotice({ bad: true, text: String(e) })
      return null
    })
    setPreviewing(false)
    if (!res) return
    setPreview({ res, key: batchKey })
  }, [collectBatch, adds, removes, batchKey])

  const save = useCallback(async () => {
    const { updates, errors } = collectBatch()
    setFieldErrors(errors)
    const bad = Object.keys(errors)
    if (bad.length > 0) {
      setNotice({ bad: true, text: `有 ${bad.length} 项填得不对，没有保存：${bad.join('、')}` })
      return
    }
    // The page's whole unsaved state goes in one call: the drafts, the items it is holding (by the
    // path it gave each one) and the items it dropped. Counted together, because any one of the
    // three is a change the reader is about to make.
    if (dirty.length === 0 && adds.length === 0 && removes.length === 0) {
      setNotice({ bad: false, text: '没有改动需要保存' })
      return
    }

    setSaving(true)
    const res = await AgentService.SaveSettings(updates, adds.map((a) => a.path), removes).catch((e) => {
      setNotice({ bad: true, text: String(e) })
      return null
    })
    setSaving(false)
    if (!res) return
    // A save that wrote re-reads (the values are the new ones, the items it added are now real and
    // the ones it dropped are gone). A save that was REFUSED does not: nothing changed anywhere, and
    // the reader's typing — and their pending items — have to survive, because they are about to fix
    // whatever the sentence named. Either way the sentence is shown as the backend wrote it (this
    // page does not translate Go errors).
    if (res.ok) {
      setAdds([])
      setRemoves([])
      // The preview described a batch that has now been written: keeping it on screen would show a
      // diff of the past next to a form that no longer holds those edits.
      setPreview(null)
      await load()
      // A save can change MORE than this page: `providers` is a config list the conversation's
      // model picker holds its own copy of. This page re-reads its schema; that copy has to be told
      // (see onSaved) — otherwise the picker only catches up on the next launch.
      onSaved?.()
    }
    setNotice({ bad: res.ok !== true, text: res.summary || '' })
  }, [collectBatch, load, dirty, adds, removes, onSaved])

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
        {/* The window's top strip, as on the sidebar: the lights' room on macOS, the drag handle
            this page otherwise has none of off macOS (the page covers the window, toolbar
            included). */}
        <div className={`window-strip${IS_MAC ? '' : ' drag-region'}`} aria-hidden="true" />
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
              : `${shownFields.length} 个可配置项`}
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
              //
              // The items a reader DROPPED are left out of the list they were drawn in — that is
              // what "删除直接把它移出列表" looks like — and the line under the ＋ is where they went.
              const gone = sec.list ? removes.filter((p) => listOf(p) === sec.list!.path) : []
              const items = sec.items
                .filter((it) => it.kind !== 'elem' || !removes.includes(it.path))
                .map((it) =>
                it.kind === 'row' ? (
                  <FieldRow {...rowProps(it.field)} label={shorten(it.field.key, sec.prefix)} />
                ) : (
                  <ElementCard key={it.path} item={it}
                    dirtyCount={it.fields.filter(isDirty).length}
                    hasError={it.fields.some((f) => fieldErrors[f.key])}
                    forceOpen={adds.some((a) => a.path === it.path)}
                    forceFolded={!!listOf(it.path) && foldedHere.has(listOf(it.path)!)}
                    sourceFile={schema?.fileSources?.[it.path]}
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
                          ? <span className="set-add-empty">{shorten(sec.list.path, sec.prefix)} 还没有条目</span>
                          : null}
                        <button className="btn" onClick={() => addItem(sec.list!.path)}>
                          ＋ 添加 {shorten(sec.list.path, sec.prefix)}
                        </button>
                        {/* What the reader dropped, where the item used to be listed — a line per
                            item, with the one way back. It says 「保存后移除」 because that is the
                            truth: nothing is written yet, which is also why a second question before
                            dropping would be asking about the wrong thing. */}
                        {gone.map((path) => (
                          <span className="set-removed" key={path}>
                            <span className="set-removed-item">{elementLocator(path, sec.prefix)}</span>
                            <span>已标记删除，保存后移除</span>
                            <button className="btn ghost" onClick={() => undoRemove(path)}>撤销</button>
                          </span>
                        ))}
                      </div>
                    ) : null}
                  </div>
                )
            })
          )}

          {/* The MCP servers editor: the one block on this page that is not a config leaf, drawn
              in the group the BACKEND names (schema.mcpServerEditor) rather than matched here by a
              name — which group a thing belongs to is a claim about the config, and this file
              knows no keys (see the file's opening comment). It writes mcp.json, not config.yaml,
              so it stays out of the batch below. */}
          {!loadError && schema && shown === schema.mcpServerEditor ? <MCPServersEditor /> : null}

          {/* The phone pairing card: also not a config leaf (its token is a credential in its
              own file), drawn in the group the BACKEND names — see schema.mobilePairing. */}
          {!loadError && schema && shown === schema.mobilePairing ? <MobilePairingCard /> : null}
        </div>

        {/* The preview: what 保存 would write, as a diff of the generated config.yaml. It sits
            between the form and the foot because that is the order of the reader's act — look, then
            press — and it is the BACKEND's own answer (the same batch, the same pre-flight, the same
            marshaller), so nothing here can drift from what the save will do. */}
        {preview ? (
          <section className="set-preview" aria-label="保存前预览">
            <div className="set-preview-head">
              <span className="set-preview-title">
                {preview.res.ok ? previewTitle(preview.res) : '这次改动保存不了'}
              </span>
              {preview.res.ok ? (
                <span className="set-preview-counts">
                  {preview.res.added > 0 ? <span className="diff-count is-add">+{preview.res.added}</span> : null}
                  {preview.res.removed > 0 ? <span className="diff-count is-del">−{preview.res.removed}</span> : null}
                </span>
              ) : null}
              <button className="btn ghost" onClick={() => setPreview(null)} title="收起预览（不影响表单）">收起</button>
            </div>
            {/* A form that moved on: the diff below describes edits that are no longer the ones on
                screen, and saying so is the only honest thing to do with it. */}
            {preview.key !== batchKey ? (
              <div className="set-preview-stale">
                <span>表单已经改过，这份预览是旧的</span>
                <button className="btn ghost" onClick={() => void openPreview()} disabled={previewing}>
                  {previewing ? '重新预览中…' : '重新预览'}
                </button>
              </div>
            ) : null}
            {/* The one thing the diff cannot carry: the file on disk is not what this process would
                write, so saving rewrites the whole document (config.Save; the original is kept as
                config.yaml.bak). Shown only when it is true. */}
            {preview.res.ok && preview.res.rewrites ? (
              <div className="set-preview-warn">
                保存会把整个 config.yaml 重新生成：你手写的注释、空行与排版不会保留。原文件会留成一份
                <span className="set-preview-path">config.yaml.bak</span>
              </div>
            ) : null}
            {!preview.res.ok ? (
              <div className="set-preview-refused">{preview.res.summary}</div>
            ) : (
              <>
                {(preview.res.hunks || []).length === 0 ? null : (
                  <div className="set-preview-file">
                    <div className="set-preview-file-head">
                      <span className="set-preview-path">config.yaml</span>
                      <span className="set-preview-counts">
                        {preview.res.added > 0 ? <span className="diff-count is-add">+{preview.res.added}</span> : null}
                        {preview.res.removed > 0 ? <span className="diff-count is-del">−{preview.res.removed}</span> : null}
                      </span>
                    </div>
                    <div className="set-preview-diff">
                      <DiffLines hunks={preview.res.hunks || []} lineNumbers />
                    </div>
                  </div>
                )}
                {/* Provider files are separate documents with the same save behind them: each one
                    gets its own heading, because the reader's question ("where does this go?") has
                    one answer per file, and the name is what they would go and open. */}
                {(preview.res.files || []).map((f) => (
                  <div className="set-preview-file" key={f.name}>
                    <div className="set-preview-file-head">
                      <span className="set-preview-path">{f.name}</span>
                      {f.new ? <span className="set-preview-tag">新文件</span> : null}
                      {f.deleted ? <span className="set-preview-tag">整个文件删除</span> : null}
                      <span className="set-preview-counts">
                        {f.added > 0 ? <span className="diff-count is-add">+{f.added}</span> : null}
                        {f.removed > 0 ? <span className="diff-count is-del">−{f.removed}</span> : null}
                      </span>
                    </div>
                    <div className="set-preview-diff">
                      <DiffLines hunks={f.hunks || []} lineNumbers />
                    </div>
                  </div>
                ))}
                {(preview.res.hunks || []).length === 0 && (preview.res.files || []).length === 0 ? (
                  <div className="set-preview-empty">这次改动不会写进文件（表单里的值和文件里的一致）</div>
                ) : null}
              </>
            )}
          </section>
        ) : null}

        <div className="settings-foot">
          <div className={`settings-notice${notice?.bad ? ' is-bad' : ''}`}>{notice?.text ?? ''}</div>
          {/* What the save will carry, counted the way the reader counts it: fields they changed,
              items they added, items they dropped. The three are one transaction, so the foot must
              not say "0 项未保存" while the form holds an item that is not in the config. */}
          {pendingText(dirty.length, adds.length, removes.length) ? (
            <div className="settings-dirty-count">{pendingText(dirty.length, adds.length, removes.length)}</div>
          ) : null}
          <button className="btn ghost" onClick={() => void openPreview()}
            disabled={saving || checking || previewing || (dirty.length === 0 && adds.length === 0 && removes.length === 0)}
            title="先看一眼这次保存会写进哪些文件（不会写文件）">
            {previewing ? '预览中…' : '预览'}
          </button>
          {/* The save button carries its own class: 预览 sits beside it, and "the first .btn in the
              foot" stopped being a way to name this one (the smoke driver clicks it by name). */}
          <button className="btn settings-save" onClick={() => void save()}
            disabled={saving || checking || (dirty.length === 0 && adds.length === 0 && removes.length === 0)}>
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
function ElementCard({ item, dirtyCount, hasError, forceOpen, forceFolded, sourceFile, onRemove, renderRow }: {
  item: Extract<Item, { kind: 'elem' }>
  dirtyCount: number
  hasError: boolean
  /** Open this card whatever the default is: an item this page has just added is where the reader
   *  is about to type, and eleven empty leaves behind a fold is a card they would have to find. It
   *  outranks forceFolded — filling in a new item is exactly what ＋ promised. */
  forceOpen?: boolean
  /** The item's list is declared folded (SettingListVO.folded): shut whatever its size. */
  forceFolded?: boolean
  /** The FILE this item lives in (`provider.deepseek.json`), for items that each come from their
   *  own file (SettingsSchemaVO.fileSources). Empty for an item the page is holding that is not in
   *  any file yet, and for every list whose items share config.yaml. */
  sourceFile?: string
  /** Removing is offered for an item of a declared list (see Section.list), and it is ONE gesture:
   *  the card leaves the list and a line at the foot of the block offers 「撤销」. That works
   *  because the removal is not written — the save is what performs it — so the confirmation a
   *  destructive button would need is nothing but a worse undo. */
  onRemove?: () => void
  renderRow: (f: SettingFieldVO, label: string) => ReactNode
}) {
  // null = follow the default, so an item that grows or shrinks still opens the way a first
  // visit would; a click pins it either way.
  const [open, setOpen] = useState<boolean | null>(null)
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
        {/* Where this item is stored. A provider lives in its own file, so a save writes a file the
            reader can name — and the fold hides the card's body, which is the only other place that
            could say it. It is the backend's answer (fileSources), not a guess from the path. */}
        {sourceFile ? <span className="set-elem-source" title={`这一项存在 ${sourceFile} 里`}>{sourceFile}</span> : null}
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
            // 危险色：这是唯一会把东西从用户配置里拿走的入口，和旁边的「撤销这一项」（中性灰）
            // 必须一眼分得开。`.ghost` 只去掉边框，颜色留在 `.danger` 上。
            <button className="btn danger ghost" onClick={onRemove}>删除这一项</button>
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

/** SecretInput is a credential's box: masked until the reader asks to see it.
 *
 *  The MASK is a display decision and not a secret — the value is in this page, the DOM and the file
 *  either way, and nothing about it is access control. What it buys is that a key is not on screen
 *  while its owner shares one, and that a screenshot of this page is not a screenshot of the key.
 *
 *  Which leaves these are is the backend's answer (`secret`, from config's own leaf names), so this
 *  component never looks at a key. The reveal is this box's own state, not a prop: whether a reader
 *  has opened one box is a fact about their screen, nothing outside the box wants it, and nothing in
 *  the draft machinery should see it (a reveal is not an edit). It therefore resets with the page —
 *  masked is the state to come back to.
 *
 *  The value is a STRING draft like every other control's, so a half-typed key behaves exactly like
 *  a half-typed model name: dirty, savable, and put back untouched by a refused save. */
function SecretInput({ value, onChange }: { value: string; onChange: (v: Draft) => void }) {
  const [shown, setShown] = useState(false)
  const label = shown ? '隐藏' : '显示明文'
  return (
    <div className="set-secret">
      <input className="set-input" type={shown ? 'text' : 'password'} value={value} spellCheck={false}
        autoComplete="off" onChange={(e) => onChange(e.target.value)} />
      <button type="button" className="set-secret-eye" onClick={() => setShown(!shown)}
        title={label} aria-label={label} aria-pressed={shown}>
        <EyeIcon open={shown} />
      </button>
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
      // The EXAMPLES come from the row that owns the key (f.listExample): a list of shell globs
      // and a list of paths are both string_list, and only the backend knows which one this box
      // is. A box that showed `git *` for `permissions.files.read.ask` was teaching the wrong
      // shape of value — the reader would type a command into a field that wants a path.
      return (
        <textarea className="set-textarea" rows={3} value={text} spellCheck={false}
          onChange={(e) => onChange(e.target.value)}
          placeholder={f.listExample ? `每行一项，例如：\n${f.listExample}` : '每行一项'} />
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
        // A credential is a box of its own: same input, masked until the reader asks (see
        // SecretInput). It can never be a dropdown — a key has no declared choice set — so the
        // two branches cannot both apply.
        if (f.secret) return <SecretInput value={text} onChange={onChange} />
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
