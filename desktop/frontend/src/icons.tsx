// One icon set, one stroke, three sizes — because "the icons look off" is not a drawing problem.
//
// This app used to draw its own: 23 inline SVGs at eight stroke widths (1.4, 1.5, 1.6, 1.7, 1.8,
// 1.9, 2, 2.5) and ten box sizes (9, 10, 11, 12, 13, 14, 15, 16, 18, 22). Every one of them was
// fine on its own; in a row along the titlebar they read as borrowed from different places, and
// that is the whole of what the eye was reporting. So the set is lucide, and the two decisions it
// needs are made HERE, once, instead of at each call site.
//
// The width is in the icon's own 24-unit box, so what the reader sees is stroke × size / 24:
// 2 draws ~1.2px at 14 and ~1.3px at 16, which is the weight the old hand-drawn 16px/1.8 glyphs
// had. Never pass strokeWidth at a call site — two decisions in two places is how this drift
// started.
//
// The three sizes follow the TEXT the icon sits beside, not taste: ICON_XS where the row is
// micro/meta (a badge, the composer's status line), ICON_SM for a control beside label/ui text,
// ICON_MD for the titlebar's buttons. An icon larger than its own row's text is what makes a
// status line look assembled rather than designed.
//
// Three marks stay hand-drawn, and none of them is an icon in this sense:
//   Caret        its box is --caret-size, and the sidebar's fold target depends on that number
//   ContextMeter / CacheRing   data graphics — the arcs carry a value, not a meaning
//   BranchChip   an 11px mark that sits inside a text row and must not grow with a hover state
import type { LucideIcon } from 'lucide-react'

export const ICON_STROKE = 2
export const ICON_XS = 12
export const ICON_SM = 14
export const ICON_MD = 16

export function Icon({ as: Glyph, size = ICON_SM, className }: {
  as: LucideIcon
  size?: number
  className?: string
}) {
  return <Glyph size={size} strokeWidth={ICON_STROKE} className={className} aria-hidden="true" />
}
