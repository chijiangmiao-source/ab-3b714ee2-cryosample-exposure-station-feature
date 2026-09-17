import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import BatchCard from '../src/components/BatchCard.vue'

const fresh = {
  barcode: 'B-100',
  allowedSeconds: 10,
  accumulatedSeconds: 0,
  state: 'in',
  status: 'usable',
  createdAt: '2026-09-13T08:00:00Z',
  lastEvent: null
}

describe('BatchCard', () => {
  it('shows in-cabinet state with takeout enabled and return disabled', () => {
    const w = mount(BatchCard, { props: { batch: fresh } })
    expect(w.get('[data-test="state-badge"]').text()).toBe('柜内')
    expect(w.get('[data-test="status-badge"]').text()).toBe('可用')
    expect(w.get('[data-test="accumulated"]').text()).toBe('0 秒')
    expect(w.get('[data-test="remaining"]').text()).toBe('10 秒')
    expect(w.get('[data-test="last-event"]').text()).toBe('尚无事件')
    expect(w.get('[data-test="conclusion"]').text()).toBe('可用')
    expect(w.get('[data-test="takeout-btn"]').attributes('disabled')).toBeUndefined()
    expect(w.get('[data-test="return-btn"]').attributes('disabled')).toBeDefined()
  })

  it('out-of-cabinet batch enables return and disables takeout', () => {
    const b = {
      ...fresh,
      state: 'out',
      lastEvent: { id: 1, type: 'takeout', at: '2026-09-13T08:00:05Z', deltaSeconds: null }
    }
    const w = mount(BatchCard, { props: { batch: b } })
    expect(w.get('[data-test="state-badge"]').text()).toBe('柜外')
    expect(w.get('[data-test="takeout-btn"]').attributes('disabled')).toBeDefined()
    expect(w.get('[data-test="return-btn"]').attributes('disabled')).toBeUndefined()
    expect(w.get('[data-test="last-event"]').text()).toContain('取出 @ 2026-09-13T08:00:05Z')
  })

  it('boundary batch (accumulated == allowed) still shows 可用 with 0 remaining', () => {
    const b = {
      ...fresh,
      accumulatedSeconds: 10,
      lastEvent: { id: 2, type: 'return', at: '2026-09-13T08:00:15Z', deltaSeconds: 10 }
    }
    const w = mount(BatchCard, { props: { batch: b } })
    expect(w.get('[data-test="remaining"]').text()).toBe('0 秒')
    expect(w.get('[data-test="conclusion"]').text()).toBe('可用')
    expect(w.get('[data-test="last-event"]').text()).toContain('（本次 +10 秒）')
    expect(w.get('[data-test="takeout-btn"]').attributes('disabled')).toBeUndefined()
  })

  it('scrapped batch shows 已报废 and disables both actions', () => {
    const b = { ...fresh, accumulatedSeconds: 11, status: 'scrapped' }
    const w = mount(BatchCard, { props: { batch: b } })
    expect(w.get('[data-test="conclusion"]').text()).toContain('已报废')
    expect(w.get('[data-test="remaining"]').text()).toBe('-1 秒')
    expect(w.get('[data-test="takeout-btn"]').attributes('disabled')).toBeDefined()
    expect(w.get('[data-test="return-btn"]').attributes('disabled')).toBeDefined()
  })

  it('emits takeout / return on button clicks', async () => {
    const w = mount(BatchCard, { props: { batch: fresh } })
    await w.get('[data-test="takeout-btn"]').trigger('click')
    expect(w.emitted('takeout')).toHaveLength(1)

    const out = { ...fresh, state: 'out' }
    const w2 = mount(BatchCard, { props: { batch: out } })
    await w2.get('[data-test="return-btn"]').trigger('click')
    expect(w2.emitted('return')).toHaveLength(1)
  })

  it('busy prop disables everything', () => {
    const w = mount(BatchCard, { props: { batch: fresh, busy: true } })
    expect(w.get('[data-test="takeout-btn"]').attributes('disabled')).toBeDefined()
  })
})
