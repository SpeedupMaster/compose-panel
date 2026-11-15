export type MenuRect = { left: number; top: number; bottom: number; width: number }
export type Viewport = { width: number; height: number }
export type MenuPlacement = {
  left: number
  top?: number
  bottom?: number
  minWidth: number
  maxWidth: number
  maxHeight: number
}

/** Compute portal dropdown bounds clamped cleanly within mobile and desktop viewports. */
export function calcMenuPlacement(
  rect: MenuRect,
  viewport: Viewport,
  opts?: { maxPreferredWidth?: number; minPreferredWidth?: number; preferHeight?: number; maxHeight?: number },
): MenuPlacement {
  const gap = 4
  const margin = 8
  const maxAvailW = Math.max(120, viewport.width - margin * 2)
  const capW = Math.min(opts?.maxPreferredWidth ?? 480, maxAvailW)
  const minWidth = Math.min(Math.max(rect.width, opts?.minPreferredWidth ?? 0), capW)
  const left = Math.max(margin, Math.min(rect.left, viewport.width - minWidth - margin))
  const maxWidth = Math.max(minWidth, Math.min(capW, viewport.width - left - margin))
  const below = viewport.height - rect.bottom - gap - margin
  const above = rect.top - gap - margin
  const want = opts?.preferHeight ?? 200
  const up = below < want && above > below
  const capH = opts?.maxHeight ?? 320
  const availH = Math.max(0, up ? above : below)
  const maxHeight = Math.min(capH, availH >= 120 ? availH : Math.max(availH, Math.min(capH, 72)))
  return {
    left,
    minWidth,
    maxWidth,
    maxHeight,
    ...(up ? { bottom: viewport.height - rect.top + gap } : { top: rect.bottom + gap }),
  }
}

/** Position a floating chart tooltip inside the SVG container, flipping left near the right edge. */
export function calcChartTipLeft(pointX: number, chartWidth: number, tipWidth = 180): number {
  const w = Math.min(tipWidth, Math.max(120, chartWidth - 8))
  const rightCandidate = pointX + 12
  if (rightCandidate + w <= chartWidth - 4) return Math.max(4, rightCandidate)
  const leftCandidate = pointX - 12 - w
  if (leftCandidate >= 4) return leftCandidate
  return Math.max(4, chartWidth - w - 4)
}

/** Find the closest timestamp sample index for a pointer X coordinate. */
export function nearestChartIndex(
  clientX: number,
  rectLeft: number,
  padL: number,
  plotWidth: number,
  ts: number[],
): number | null {
  if (ts.length === 0 || plotWidth <= 0) return null
  const t0 = ts[0]
  const span = Math.max(ts[ts.length - 1] - t0, 60)
  const t = t0 + ((clientX - rectLeft - padL) / plotWidth) * span
  let best = 0
  for (let i = 1; i < ts.length; i++) {
    if (Math.abs(ts[i] - t) < Math.abs(ts[best] - t)) best = i
  }
  return best
}
