import { describe, expect, it } from 'vitest'
import { formatZ, isValidZ, nowZ, Z_TIME_RE } from '../src/lib/time'

describe('time helpers', () => {
  it('nowZ returns RFC3339 whole seconds with Z', () => {
    expect(nowZ()).toMatch(Z_TIME_RE)
  })

  it('formatZ truncates to whole seconds and uses Z', () => {
    expect(formatZ(new Date('2026-09-13T10:20:30.987Z'))).toBe('2026-09-13T10:20:30Z')
  })

  it('accepts canonical values', () => {
    expect(isValidZ('2026-09-13T10:00:00Z')).toBe(true)
    expect(isValidZ('2024-02-29T23:59:59Z')).toBe(true) // leap day
  })

  it('rejects offsets, fractions and malformed values', () => {
    expect(isValidZ('2026-09-13T10:00:00+08:00')).toBe(false)
    expect(isValidZ('2026-09-13T10:00:00.5Z')).toBe(false)
    expect(isValidZ('2026-09-13T10:00:00.000Z')).toBe(false)
    expect(isValidZ('2026-09-13 10:00:00')).toBe(false)
    expect(isValidZ('2026-09-13T10:00:00')).toBe(false)
    expect(isValidZ('2026-13-01T00:00:00Z')).toBe(false) // month 13
    expect(isValidZ('2026-02-30T00:00:00Z')).toBe(false) // not a real day
    expect(isValidZ('tomorrow')).toBe(false)
    expect(isValidZ('')).toBe(false)
    expect(isValidZ(null)).toBe(false)
  })
})
