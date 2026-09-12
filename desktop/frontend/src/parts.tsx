// The part renderer: one turn part → one piece of the transcript.
//
// Extracted from App.tsx so the side-channel panel (oneoff.tsx) replays a one-off run with
// the VERY SAME pieces the conversation uses — the same thinking row, tool card, fragment
// diff and markdown. The panel's whole premise (docs/2026-09-12-desktop-oneoff-panel-design.md
// §2.2) is that a record line IS a session message, so it would be self-defeating to render
// it with a second implementation that only looks similar.
//
// Memoized for the same reason App.tsx memoizes its bubbles: streaming replaces only the
// message being written, so every already-rendered part — and its parsed markdown — is
// skipped on each frame.

import { memo } from 'react'
import type { Part } from './types'
import { MarkdownBlock } from './markdown'
import { FileCard, fileFromSendFileArgs } from './filepreview'
import { NoticePart, ThinkingPart, ToolCard } from './components'

export const TurnPart = memo(function TurnPart({ part, workDir, onToggleDiff }: {
  part: Part
  workDir: string
  // Present only where the caller owns the open/closed state of the turn's diffs (the
  // transcript's footer chip does); a replay leaves it undefined and each card folds itself.
  onToggleDiff?: () => void
}) {
  if (part.type === 'thinking') return <ThinkingPart text={part.text || ''} />
  if (part.type === 'notice') return <NoticePart part={part} />
  if (part.type === 'tool') {
    // A SendFile call IS the attachment — show the file card rather than a raw
    // tool card (covers the live turn and reloaded history alike). workDir
    // resolves the relative paths a model sometimes writes.
    if (part.name === 'SendFile') {
      const file = fileFromSendFileArgs(part.args || '')
      if (file) return <FileCard file={file} workDir={workDir} />
    }
    return <ToolCard name={part.name || ''} title={part.title} args={part.args} summary={part.summary || ''} ok={!!part.ok}
      done={part.done} change={part.change} diffOpen={part.diffOpen} durationMs={part.durationMs}
      defaultExpanded={part.expand} onToggleDiff={onToggleDiff} />
  }
  return <MarkdownBlock text={part.text || ''} workDir={workDir} />
})
