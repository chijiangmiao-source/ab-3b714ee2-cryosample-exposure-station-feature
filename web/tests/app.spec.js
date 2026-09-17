import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import App from '../src/App.vue'
import { api } from '../src/api'

vi.mock('../src/api', () => ({
  api: {
    getBatch: vi.fn(),
    listEvents: vi.fn(),
    createBatch: vi.fn(),
    postEvent: vi.fn(),
    revokeEvent: vi.fn()
  }
}))

const freshBatch = {
  barcode: 'B-1',
  allowedSeconds: 10,
  accumulatedSeconds: 0,
  state: 'in',
  status: 'usable',
  usable: true,
  remainingSeconds: 10,
  createdAt: '2026-09-13T08:00:00Z',
  lastEvent: null
}

function apiError(status, code, message) {
  const e = new Error(message)
  e.status = status
  e.code = code
  return e
}

async function scan(w, code) {
  await w.get('[data-test="barcode-input"]').setValue(code)
  await w.get('[data-test="scan-form"]').trigger('submit')
  await flushPromises()
}

beforeEach(() => {
  vi.clearAllMocks()
  localStorage.clear()
  api.listEvents.mockResolvedValue({ events: [] })
})

describe('App', () => {
  it('scanning an unknown barcode offers the create form; creating shows the batch', async () => {
    api.getBatch.mockRejectedValue(apiError(404, 'not_found', 'no batch with this barcode'))
    api.createBatch.mockResolvedValue(freshBatch)

    const w = mount(App)
    await scan(w, 'B-1')
    expect(w.find('[data-test="create-form"]').exists()).toBe(true)

    await w.get('[data-test="create-allowed"]').setValue(10)
    await w.get('[data-test="create-created-at"]').setValue('2026-09-13T08:00:00Z')
    await w.get('[data-test="create-form"]').trigger('submit')
    await flushPromises()

    expect(api.createBatch).toHaveBeenCalledWith({
      barcode: 'B-1',
      allowedSeconds: 10,
      createdAt: '2026-09-13T08:00:00Z'
    })
    expect(w.get('[data-test="state-badge"]').text()).toBe('柜内')
    expect(w.get('[data-test="accumulated"]').text()).toBe('0 秒')
    expect(localStorage.getItem('lastBarcode')).toBe('B-1')
  })

  it('rejects invalid create input locally', async () => {
    api.getBatch.mockRejectedValue(apiError(404, 'not_found', 'nope'))
    const w = mount(App)
    await scan(w, 'B-1')
    await w.get('[data-test="create-created-at"]').setValue('2026-09-13 08:00:00')
    await w.get('[data-test="create-form"]').trigger('submit')
    await flushPromises()
    expect(api.createBatch).not.toHaveBeenCalled()
    expect(w.get('[data-test="error-banner"]').text()).toContain('RFC3339')
  })

  it('takeout then return updates the displayed state', async () => {
    const outBatch = {
      ...freshBatch,
      state: 'out',
      lastEvent: { id: 1, type: 'takeout', at: '2026-09-13T08:00:05Z', deltaSeconds: null }
    }
    const returnedBatch = {
      ...freshBatch,
      accumulatedSeconds: 10,
      remainingSeconds: 0,
      lastEvent: { id: 2, type: 'return', at: '2026-09-13T08:00:15Z', deltaSeconds: 10 }
    }
    api.getBatch.mockResolvedValue(freshBatch)
    api.postEvent.mockResolvedValueOnce(outBatch).mockResolvedValueOnce(returnedBatch)

    const w = mount(App)
    await scan(w, 'B-1')
    expect(w.get('[data-test="state-badge"]').text()).toBe('柜内')

    await w.get('[data-test="event-time"]').setValue('2026-09-13T08:00:05Z')
    await w.get('[data-test="takeout-btn"]').trigger('click')
    await flushPromises()
    expect(api.postEvent).toHaveBeenNthCalledWith(1, 'B-1', { type: 'takeout', at: '2026-09-13T08:00:05Z' })
    expect(w.get('[data-test="state-badge"]').text()).toBe('柜外')

    await w.get('[data-test="event-time"]').setValue('2026-09-13T08:00:15Z')
    await w.get('[data-test="return-btn"]').trigger('click')
    await flushPromises()
    expect(api.postEvent).toHaveBeenNthCalledWith(2, 'B-1', { type: 'return', at: '2026-09-13T08:00:15Z' })
    expect(w.get('[data-test="accumulated"]').text()).toBe('10 秒')
    expect(w.get('[data-test="remaining"]').text()).toBe('0 秒')
    expect(w.get('[data-test="conclusion"]').text()).toBe('可用')
  })

  it('409 on a stale/duplicate action shows the error and reloads truth', async () => {
    // Second tab scenario: our view says "out", but the batch was already returned.
    const outBatch = { ...freshBatch, state: 'out' }
    const alreadyReturned = { ...freshBatch, accumulatedSeconds: 10, remainingSeconds: 0 }
    api.getBatch.mockResolvedValueOnce(outBatch) // initial scan
    api.postEvent.mockRejectedValue(apiError(409, 'invalid_transition', 'batch is not out of the cabinet'))
    api.getBatch.mockResolvedValueOnce(alreadyReturned) // reload after conflict

    const w = mount(App)
    await scan(w, 'B-1')
    await w.get('[data-test="event-time"]').setValue('2026-09-13T08:00:20Z')
    await w.get('[data-test="return-btn"]').trigger('click')
    await flushPromises()

    expect(w.get('[data-test="error-banner"]').text()).toContain('invalid_transition')
    expect(w.get('[data-test="accumulated"]').text()).toBe('10 秒')
    expect(w.get('[data-test="state-badge"]').text()).toBe('柜内')
  })

  it('rejects a malformed event time without calling the API', async () => {
    api.getBatch.mockResolvedValue(freshBatch)
    const w = mount(App)
    await scan(w, 'B-1')
    await w.get('[data-test="event-time"]').setValue('2026-09-13T08:00:05+08:00')
    await w.get('[data-test="takeout-btn"]').trigger('click')
    await flushPromises()
    expect(api.postEvent).not.toHaveBeenCalled()
    expect(w.get('[data-test="error-banner"]').text()).toContain('RFC3339')
  })

  it('restores the last scanned batch after a page refresh', async () => {
    localStorage.setItem('lastBarcode', 'B-9')
    api.getBatch.mockResolvedValue({ ...freshBatch, barcode: 'B-9' })
    const w = mount(App)
    await flushPromises()
    expect(api.getBatch).toHaveBeenCalledWith('B-9')
    expect(w.get('[data-test="card-barcode"]').text()).toBe('B-9')
  })

  it('offers revocation only next to the latest active event', async () => {
    const events = [
      { id: 1, type: 'takeout', at: '2026-09-13T08:00:05Z', deltaSeconds: null },
      { id: 2, type: 'return', at: '2026-09-13T08:00:15Z', deltaSeconds: 10 }
    ]
    api.getBatch.mockResolvedValue(freshBatch)
    api.listEvents.mockResolvedValue({ events })
    const w = mount(App)
    await scan(w, 'B-1')

    // Only the latest active event (id 2) shows a revoke button.
    expect(w.find('[data-test="revoke-btn-1"]').exists()).toBe(false)
    expect(w.find('[data-test="revoke-btn-2"]').exists()).toBe(true)
    expect(w.get('[data-test="revoke-form"]').text()).toContain('#2')
  })

  it('revoking a mistaken return calls the API with at and reason and shows audit', async () => {
    const scrapped = {
      ...freshBatch,
      state: 'in',
      status: 'scrapped',
      usable: false,
      accumulatedSeconds: 11,
      remainingSeconds: -1
    }
    const restored = {
      ...freshBatch,
      state: 'out',
      accumulatedSeconds: 0,
      remainingSeconds: 10,
      lastEvent: { id: 1, type: 'takeout', at: '2026-09-13T08:00:05Z', deltaSeconds: null }
    }
    const eventsBefore = [
      { id: 1, type: 'takeout', at: '2026-09-13T08:00:05Z', deltaSeconds: null },
      { id: 2, type: 'return', at: '2026-09-13T08:00:16Z', deltaSeconds: 11 }
    ]
    const eventsAfter = [
      { id: 1, type: 'takeout', at: '2026-09-13T08:00:05Z', deltaSeconds: null },
      {
        id: 2,
        type: 'return',
        at: '2026-09-13T08:00:16Z',
        deltaSeconds: 11,
        revokedAt: '2026-09-13T08:00:20Z',
        revokeReason: '误扫归还'
      }
    ]
    api.getBatch.mockResolvedValueOnce(scrapped)
    api.listEvents.mockResolvedValueOnce({ events: eventsBefore })
    api.revokeEvent.mockResolvedValue(restored)
    api.listEvents.mockResolvedValueOnce({ events: eventsAfter })

    const w = mount(App)
    await scan(w, 'B-1')
    await w.get('[data-test="revoke-time"]').setValue('2026-09-13T08:00:20Z')
    await w.get('[data-test="revoke-reason"]').setValue('误扫归还')
    await w.get('[data-test="revoke-form"]').trigger('submit')
    await flushPromises()

    expect(api.revokeEvent).toHaveBeenCalledWith('B-1', 2, {
      at: '2026-09-13T08:00:20Z',
      reason: '误扫归还'
    })
    // Aggregate restored server-side: out of cabinet, exposure clawed back.
    expect(w.get('[data-test="state-badge"]').text()).toBe('柜外')
    expect(w.get('[data-test="accumulated"]').text()).toBe('0 秒')
    // Audit result shown on the event row.
    expect(w.get('[data-test="revoked-tag"]').text()).toBe('已撤销')
    expect(w.get('[data-test="event-row-2"]').text()).toContain('2026-09-13T08:00:20Z')
    expect(w.get('[data-test="event-row-2"]').text()).toContain('误扫归还')
    // Revoked row no longer offers a revoke button; form now targets event 1.
    expect(w.find('[data-test="revoke-btn-2"]').exists()).toBe(false)
    expect(w.get('[data-test="revoke-form"]').text()).toContain('#1')
  })

  it('revoking a takeout is supported and clears accumulated-free state', async () => {
    const out = {
      ...freshBatch,
      state: 'out',
      lastEvent: { id: 1, type: 'takeout', at: '2026-09-13T08:00:05Z', deltaSeconds: null }
    }
    const backIn = { ...freshBatch, lastEvent: null }
    api.getBatch.mockResolvedValueOnce(out)
    api.listEvents.mockResolvedValueOnce({
      events: [{ id: 1, type: 'takeout', at: '2026-09-13T08:00:05Z', deltaSeconds: null }]
    })
    api.revokeEvent.mockResolvedValue(backIn)
    api.listEvents.mockResolvedValueOnce({
      events: [{
        id: 1, type: 'takeout', at: '2026-09-13T08:00:05Z', deltaSeconds: null,
        revokedAt: '2026-09-13T08:00:09Z', revokeReason: '误扫取出'
      }]
    })

    const w = mount(App)
    await scan(w, 'B-1')
    await w.get('[data-test="revoke-time"]').setValue('2026-09-13T08:00:09Z')
    await w.get('[data-test="revoke-reason"]').setValue('误扫取出')
    await w.get('[data-test="revoke-btn-1"]').trigger('click')
    await flushPromises()

    expect(api.revokeEvent).toHaveBeenCalledWith('B-1', 1, {
      at: '2026-09-13T08:00:09Z',
      reason: '误扫取出'
    })
    expect(w.get('[data-test="state-badge"]').text()).toBe('柜内')
    expect(w.find('[data-test="revoke-form"]').exists()).toBe(false, 'no active event left')
  })

  it('requires a Z time and non-empty reason before calling the API', async () => {
    api.getBatch.mockResolvedValue(freshBatch)
    api.listEvents.mockResolvedValue({
      events: [{ id: 1, type: 'takeout', at: '2026-09-13T08:00:05Z', deltaSeconds: null }]
    })
    const w = mount(App)
    await scan(w, 'B-1')

    await w.get('[data-test="revoke-reason"]').setValue('x')
    await w.get('[data-test="revoke-time"]').setValue('2026-09-13 08:00:20')
    await w.get('[data-test="revoke-form"]').trigger('submit')
    await flushPromises()
    expect(api.revokeEvent).not.toHaveBeenCalled()
    expect(w.get('[data-test="error-banner"]').text()).toContain('RFC3339')

    await w.get('[data-test="revoke-time"]').setValue('2026-09-13T08:00:20Z')
    await w.get('[data-test="revoke-reason"]').setValue('   ')
    await w.get('[data-test="revoke-form"]').trigger('submit')
    await flushPromises()
    expect(api.revokeEvent).not.toHaveBeenCalled()
    expect(w.get('[data-test="error-banner"]').text()).toContain('撤销原因')
  })

  it('409 on revoke (non-last/duplicate/concurrency) shows conflict and reloads truth', async () => {
    const outStale = {
      ...freshBatch,
      state: 'out',
      lastEvent: { id: 3, type: 'takeout', at: '2026-09-13T08:00:30Z', deltaSeconds: null }
    }
    // Another station already revoked id 3 and continued: authoritative truth.
    const truth = {
      ...freshBatch,
      state: 'in',
      accumulatedSeconds: 10,
      remainingSeconds: 0,
      lastEvent: { id: 2, type: 'return', at: '2026-09-13T08:00:20Z', deltaSeconds: 10 }
    }
    const truthEvents = [
      { id: 1, type: 'takeout', at: '2026-09-13T08:00:10Z', deltaSeconds: null },
      { id: 2, type: 'return', at: '2026-09-13T08:00:20Z', deltaSeconds: 10 },
      {
        id: 3, type: 'takeout', at: '2026-09-13T08:00:30Z', deltaSeconds: null,
        revokedAt: '2026-09-13T08:00:40Z', revokeReason: '另一工位已撤销'
      }
    ]
    api.getBatch.mockResolvedValueOnce(outStale)
    api.listEvents.mockResolvedValueOnce({
      events: [
        { id: 1, type: 'takeout', at: '2026-09-13T08:00:10Z', deltaSeconds: null },
        { id: 2, type: 'return', at: '2026-09-13T08:00:20Z', deltaSeconds: 10 },
        { id: 3, type: 'takeout', at: '2026-09-13T08:00:30Z', deltaSeconds: null }
      ]
    })
    api.revokeEvent.mockRejectedValue(apiError(409, 'already_revoked', 'event 3 has already been revoked'))
    api.getBatch.mockResolvedValueOnce(truth)
    api.listEvents.mockResolvedValueOnce({ events: truthEvents })

    const w = mount(App)
    await scan(w, 'B-1')
    await w.get('[data-test="revoke-time"]').setValue('2026-09-13T08:00:50Z')
    await w.get('[data-test="revoke-reason"]').setValue('再撤销一次')
    await w.get('[data-test="revoke-form"]').trigger('submit')
    await flushPromises()

    expect(w.get('[data-test="error-banner"]').text()).toContain('already_revoked')
    // Reloaded authoritative aggregate/state without mutating anything locally.
    expect(w.get('[data-test="state-badge"]').text()).toBe('柜内')
    expect(w.get('[data-test="accumulated"]').text()).toBe('10 秒')
    expect(w.get('[data-test="event-row-3"]').text()).toContain('已撤销')
    expect(w.find('[data-test="revoke-btn-3"]').exists()).toBe(false)
  })
})
