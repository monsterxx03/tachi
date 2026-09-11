// A tool call's change, rendered as a fragment diff.
//
// The hunks are computed in Go (pkg/linediff) so this module only decides how they
// look. Three rules carry the meaning:
//
//   - add/del get a ± prefix AND a colour, never colour alone (accessibility, and it
//     survives a bad monitor);
//   - no line numbers: the numbers we have are fragment-relative, and a fragment has
//     no position in the file — showing 1,2,3 would read as file coordinates;
//   - a long diff folds. WriteFile is all-add (the whole file) and a replace_all can
//     be long, but the card's job is to show WHAT changed; the file itself is one
//     click away (打开 / 预览).

import { memo, useState } from 'react'
import type { FileChangeVO } from '../bindings/github.com/monsterxx03/tachi/desktop'

// DIFF_FOLD_LINES is how much of a change shows before folding.
const DIFF_FOLD_LINES = 24

export const DiffBlock = memo(function DiffBlock({ change }: { change: FileChangeVO }) {
  const [open, setOpen] = useState(false)
  const hunks = change.hunks || []
  const folded = !open && hunks.length > DIFF_FOLD_LINES
  const shown = folded ? hunks.slice(0, DIFF_FOLD_LINES) : hunks

  return (
    <div className="diff-block">
      <div className="diff-head">
        <span className="diff-path" title={change.path}>{change.path}</span>
        {change.added > 0 ? <span className="diff-count is-add">+{change.added}</span> : null}
        {change.removed > 0 ? <span className="diff-count is-del">−{change.removed}</span> : null}
        {/* A fragment diff can only show one block against another, so a call that
            replaced every occurrence says so instead of implying paired changes. */}
        {change.replaceAll ? <span className="diff-note">多处替换，整段对照</span> : null}
      </div>
      <div className="diff-body">
        {shown.map((h, i) => (
          <div key={i} className={`diff-line is-${h.kind}`}>
            <span className="diff-sign">{h.kind === 'add' ? '+' : h.kind === 'del' ? '−' : ' '}</span>
            {/* An empty line still needs to occupy a row. */}
            <span className="diff-text">{h.text === '' ? ' ' : h.text}</span>
          </div>
        ))}
        {folded ? (
          <button type="button" className="diff-more" onClick={() => setOpen(true)}>
            还有 {hunks.length - DIFF_FOLD_LINES} 行，展开
          </button>
        ) : null}
      </div>
    </div>
  )
})
