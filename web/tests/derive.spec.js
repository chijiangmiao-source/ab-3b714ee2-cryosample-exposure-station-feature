import { describe, expect, it } from 'vitest'
import {
  canReturn,
  canTakeout,
  eventTypeLabel,
  isUsable,
  remainingSeconds,
  stateLabel,
  statusLabel
} from '../src/lib/derive'

const base = {
  barcode: 'B-1',
  allowedSeconds: 10,
  accumulatedSeconds: 0,
  state: 'in',
  status: 'usable'
}

describe('derive', () => {
  it('fresh batch: in cabinet, takeout allowed, return not', () => {
    expect(canTakeout(base)).toBe(true)
    expect(canReturn(base)).toBe(false)
    expect(stateLabel(base)).toBe('柜内')
    expect(statusLabel(base)).toBe('可用')
    expect(isUsable(base)).toBe(true)
  })

  it('out batch: return allowed, takeout not', () => {
    const b = { ...base, state: 'out' }
    expect(canTakeout(b)).toBe(false)
    expect(canReturn(b)).toBe(true)
    expect(stateLabel(b)).toBe('柜外')
  })

  it('boundary: accumulated == allowed is still usable with 0 remaining', () => {
    const b = { ...base, accumulatedSeconds: 10 }
    expect(remainingSeconds(b)).toBe(0)
    expect(isUsable(b)).toBe(true)
    expect(canTakeout(b)).toBe(true)
  })

  it('over limit: scrapped batches can never be taken out', () => {
    const b = { ...base, accumulatedSeconds: 11, status: 'scrapped' }
    expect(remainingSeconds(b)).toBe(-1)
    expect(isUsable(b)).toBe(false)
    expect(statusLabel(b)).toBe('已报废')
    expect(canTakeout(b)).toBe(false)
    expect(canReturn(b)).toBe(false)
  })

  it('null batch is safe', () => {
    expect(canTakeout(null)).toBe(false)
    expect(canReturn(null)).toBe(false)
    expect(stateLabel(null)).toBe('')
  })

  it('event type labels', () => {
    expect(eventTypeLabel('takeout')).toBe('取出')
    expect(eventTypeLabel('return')).toBe('归还')
  })
})
