import test from 'node:test'
import assert from 'node:assert/strict'
import { calcChartTipLeft, calcMenuPlacement, nearestChartIndex } from '../src/components/geometry.ts'

test('calcMenuPlacement keeps combobox width inside 375px phone viewport', () => {
  const pos = calcMenuPlacement(
    { left: 24, top: 180, bottom: 216, width: 327 },
    { width: 375, height: 667 },
    { maxPreferredWidth: 480, minPreferredWidth: 320 },
  )
  assert.ok(pos.left >= 8)
  assert.ok(pos.left + pos.minWidth <= 375 - 8)
  assert.ok(pos.left + pos.maxWidth <= 375 - 8)
  assert.ok(pos.top !== undefined)
})

test('calcMenuPlacement clamps right-aligned trigger back into viewport', () => {
  const pos = calcMenuPlacement(
    { left: 280, top: 100, bottom: 130, width: 104 },
    { width: 390, height: 844 },
    { maxPreferredWidth: 480 },
  )
  assert.ok(pos.left + pos.minWidth <= 390 - 8)
  assert.ok(pos.left >= 8)
})

test('calcMenuPlacement flips above trigger when bottom space is limited', () => {
  const pos = calcMenuPlacement(
    { left: 16, top: 680, bottom: 716, width: 200 },
    { width: 390, height: 844 },
    { preferHeight: 200 },
  )
  assert.equal(pos.top, undefined)
  assert.ok(pos.bottom !== undefined && pos.bottom > 0)
  assert.ok(pos.maxHeight <= 320)
})

test('calcMenuPlacement caps height within tight vertical viewport', () => {
  const pos = calcMenuPlacement(
    { left: 16, top: 80, bottom: 116, width: 200 },
    { width: 390, height: 220 },
    { preferHeight: 200 },
  )
  assert.ok(pos.top !== undefined)
  assert.ok(pos.maxHeight <= 220 - 116 - 4 - 8)
})

test('calcChartTipLeft never returns negative left on narrow charts', () => {
  const left = calcChartTipLeft(120, 180, 180)
  assert.ok(left >= 4, `expected left >= 4, got ${left}`)
  assert.ok(left + 164 <= 180, `expected tip to fit chart width, left=${left}`)
})

test('calcChartTipLeft flips left when point is near right edge on mobile', () => {
  const left = calcChartTipLeft(310, 340, 180)
  assert.ok(left >= 4)
  assert.ok(left + 180 <= 340 - 4)
  assert.ok(left < 310)
})

test('nearestChartIndex picks closest timestamp and handles empty', () => {
  assert.equal(nearestChartIndex(100, 0, 50, 200, []), null)
  const ts = [100, 200, 300]
  assert.equal(nearestChartIndex(50, 0, 50, 200, ts), 0)
  assert.equal(nearestChartIndex(155, 0, 50, 200, ts), 1)
  assert.equal(nearestChartIndex(245, 0, 50, 200, ts), 2)
})
