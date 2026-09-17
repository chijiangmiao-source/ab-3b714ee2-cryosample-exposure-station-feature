import { expect, test } from '@playwright/test'

// Unique barcodes per run so the suite is repeatable against the same DB.
const uniq = Date.now()
let seq = 0
function nextBarcode(prefix) {
  seq += 1
  return `E2E-${prefix}-${uniq}-${seq}`
}

const T0 = '2026-01-01T00:00:00Z'

async function createBatch(page, barcode, allowedSeconds, createdAt = T0) {
  await page.goto('/')
  await page.getByTestId('barcode-input').fill(barcode)
  await page.getByTestId('lookup-btn').click()
  await expect(page.getByTestId('create-form')).toBeVisible()
  await page.getByTestId('create-allowed').fill(String(allowedSeconds))
  await page.getByTestId('create-created-at').fill(createdAt)
  await page.getByTestId('create-submit').click()
  await expect(page.getByTestId('batch-card')).toBeVisible()
}

async function scan(page, barcode) {
  await page.goto('/')
  await page.getByTestId('barcode-input').fill(barcode)
  await page.getByTestId('lookup-btn').click()
  await expect(page.getByTestId('batch-card')).toBeVisible()
}

async function takeout(page, at) {
  await page.getByTestId('event-time').fill(at)
  await page.getByTestId('takeout-btn').click()
}

async function doReturn(page, at) {
  await page.getByTestId('event-time').fill(at)
  await page.getByTestId('return-btn').click()
}

async function undoLast(page, at, reason) {
  await page.getByTestId('undo-btn').click()
  await page.getByTestId('undo-time').fill(at)
  await page.getByTestId('undo-reason').fill(reason)
  await page.getByTestId('undo-confirm').click()
}

// 主流程：误扫归还（时刻偏晚）导致超限报废，撤销后恢复柜外与原累计，
// 再按正确时刻重新归还。
test('误归还导致报废：撤销后恢复柜外与原累计，并可再次正确归还', async ({ page }) => {
  const barcode = nextBarcode('UNDO-RETURN')
  await createBatch(page, barcode, 10)

  await takeout(page, '2026-01-01T00:00:05Z')
  await doReturn(page, '2026-01-01T00:00:16Z') // +11 秒 > 上限 10：误扫导致报废
  await expect(page.getByTestId('status-badge')).toHaveText('已报废')
  await expect(page.getByTestId('accumulated')).toHaveText('11 秒')
  await expect(page.getByTestId('takeout-btn')).toBeDisabled()
  await expect(page.getByTestId('return-btn')).toBeDisabled()

  // 撤销误扫的归还：批次恢复柜外、累计扣回、重新判定为可用
  await undoLast(page, '2026-01-01T00:00:20Z', '误扫归还时间')
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')
  await expect(page.getByTestId('status-badge')).toHaveText('可用')
  await expect(page.getByTestId('accumulated')).toHaveText('0 秒')
  await expect(page.getByTestId('remaining')).toHaveText('10 秒')
  await expect(page.getByTestId('return-btn')).toBeEnabled()
  await expect(page.getByTestId('info-banner')).toContainText('已撤销')

  // 审计结果留在事件行上（时刻 + 原因），被撤销行不再提供撤销按钮
  await expect(page.getByTestId('events-table')).toContainText('已撤销 @ 2026-01-01T00:00:20Z')
  await expect(page.getByTestId('events-table')).toContainText('误扫归还时间')
  await expect(page.getByTestId('undo-btn')).toHaveCount(1) // 仅剩最早那条取出可撤销

  // 刷新后撤销结果与审计信息仍在（服务端持久化）
  await page.reload()
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')
  await expect(page.getByTestId('accumulated')).toHaveText('0 秒')
  await expect(page.getByTestId('events-table')).toContainText('已撤销 @ 2026-01-01T00:00:20Z')

  // 按正确时刻重新归还：+9 秒 ≤ 上限，批次可用
  await doReturn(page, '2026-01-01T00:00:14Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page.getByTestId('accumulated')).toHaveText('9 秒')
  await expect(page.getByTestId('conclusion')).toHaveText('可用')
})

test('误取出可撤销，批次回到柜内并可重新取出', async ({ page }) => {
  const barcode = nextBarcode('UNDO-TAKEOUT')
  await createBatch(page, barcode, 100)

  await takeout(page, '2026-01-01T00:00:05Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')

  await undoLast(page, '2026-01-01T00:00:06Z', '误扫取出')
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page.getByTestId('accumulated')).toHaveText('0 秒')
  await expect(page.getByTestId('last-event')).toHaveText('尚无事件')
  await expect(page.getByTestId('events-table')).toContainText('已撤销 @ 2026-01-01T00:00:06Z')
  await expect(page.getByTestId('events-table')).toContainText('误扫取出')
  // 唯一事件已被撤销，没有可再撤销的记录
  await expect(page.getByTestId('undo-btn')).toHaveCount(0)

  // 重复撤销同一事件（绕过界面直接调接口）→ 409 already_undone
  const events = await (await page.request.get(`/api/batches/${barcode}/events`)).json()
  const res = await page.request.post(`/api/batches/${barcode}/events/${events.events[0].id}/undo`, {
    data: { at: '2026-01-01T00:00:07Z', reason: '重复撤销' }
  })
  expect(res.status()).toBe(409)
  expect((await res.json()).error.code).toBe('already_undone')

  // 可继续正常取出
  await takeout(page, '2026-01-01T00:00:07Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')
})

test('非最近事件不能撤销，撤销时刻早于最后操作被拒绝', async ({ page }) => {
  const barcode = nextBarcode('UNDO-ORDER')
  await createBatch(page, barcode, 100)
  await takeout(page, '2026-01-01T00:00:05Z')
  await doReturn(page, '2026-01-01T00:00:10Z')

  // 页面上只有最近一条事件提供撤销按钮
  await expect(page.getByTestId('undo-btn')).toHaveCount(1)

  const events = await (await page.request.get(`/api/batches/${barcode}/events`)).json()
  const [takeoutEv, returnEv] = events.events

  // 直接调接口撤销较早的取出事件 → 409 not_last_event
  let res = await page.request.post(`/api/batches/${barcode}/events/${takeoutEv.id}/undo`, {
    data: { at: '2026-01-01T00:00:11Z', reason: '尝试撤销非最近事件' }
  })
  expect(res.status()).toBe(409)
  expect((await res.json()).error.code).toBe('not_last_event')

  // 撤销时刻早于批次最后操作 → 409 time_not_monotonic
  res = await page.request.post(`/api/batches/${barcode}/events/${returnEv.id}/undo`, {
    data: { at: '2026-01-01T00:00:09Z', reason: '时刻早于最后操作' }
  })
  expect(res.status()).toBe(409)
  expect((await res.json()).error.code).toBe('time_not_monotonic')

  // 空原因 → 400 reason_required
  res = await page.request.post(`/api/batches/${barcode}/events/${returnEv.id}/undo`, {
    data: { at: '2026-01-01T00:00:11Z', reason: '  ' }
  })
  expect(res.status()).toBe(400)
  expect((await res.json()).error.code).toBe('reason_required')

  // 全部拒绝后状态不变：累计、柜内外状态与流水均未改变
  await page.reload()
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page.getByTestId('accumulated')).toHaveText('5 秒')
  await expect(page.getByTestId('events-table')).not.toContainText('已撤销')
})

test('两个工位并发撤销同一事件，仅一个成功', async ({ page, context }) => {
  const barcode = nextBarcode('UNDO-RACE')
  await createBatch(page, barcode, 100)
  await takeout(page, '2026-01-01T00:00:05Z')
  await doReturn(page, '2026-01-01T00:00:10Z')

  // 第二个工位同时打开同一批次，两个工位都准备撤销同一条归还
  const page2 = await context.newPage()
  await scan(page2, barcode)

  const undoStatuses = []
  for (const p of [page, page2]) {
    p.on('response', (r) => {
      if (r.url().includes('/undo')) undoStatuses.push(r.status())
    })
    await p.getByTestId('undo-btn').click()
    await p.getByTestId('undo-time').fill('2026-01-01T00:00:11Z')
    await p.getByTestId('undo-reason').fill('误扫归还')
  }
  await Promise.all([
    page.getByTestId('undo-confirm').click(),
    page2.getByTestId('undo-confirm').click()
  ])

  // 一个 200，一个 409
  await expect.poll(() => undoStatuses.length).toBe(2)
  expect(undoStatuses.sort()).toEqual([200, 409])

  // 两个页面最终都呈现撤销后的权威状态（失败方已重新载入）
  for (const p of [page, page2]) {
    await expect(p.getByTestId('state-badge')).toHaveText('柜外')
    await expect(p.getByTestId('accumulated')).toHaveText('0 秒')
    await expect(p.getByTestId('events-table')).toContainText('已撤销 @ 2026-01-01T00:00:11Z')
  }

  // 暴露只被扣回一次，流水里只有一条撤销审计
  await page.reload()
  await expect(page.getByTestId('accumulated')).toHaveText('0 秒')
  await expect(page.getByTestId('events-table').getByText('已撤销 @')).toHaveCount(1)
  await page2.close()
})

test('撤销操作被拒绝后重新载入服务端状态', async ({ page }) => {
  const barcode = nextBarcode('UNDO-RELOAD')
  await createBatch(page, barcode, 100)
  await takeout(page, '2026-01-01T00:00:05Z')

  // 撤销时刻早于取出时刻 → 409 time_not_monotonic
  await undoLast(page, '2026-01-01T00:00:04Z', '误扫取出')
  await expect(page.getByTestId('error-banner')).toContainText('time_not_monotonic')

  // 失败后页面已重新载入权威状态：仍在柜外、累计 0、流水无撤销
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')
  await expect(page.getByTestId('accumulated')).toHaveText('0 秒')
  await expect(page.getByTestId('events-table')).not.toContainText('已撤销')
})
