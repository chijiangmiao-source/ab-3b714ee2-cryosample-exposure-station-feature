import { expect, test } from '@playwright/test'

// Unique barcodes per run so the suite is repeatable against the same DB.
const uniq = Date.now()
let seq = 0
function nextBarcode(prefix) {
  seq += 1
  return `E2E-REV-${prefix}-${uniq}-${seq}`
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

async function revoke(page, at, reason) {
  await page.getByTestId('revoke-time').fill(at)
  await page.getByTestId('revoke-reason').fill(reason)
  await page.getByTestId('revoke-submit').click()
}

// Event ids are globally auto-incremented across batches; read the actual ids
// off the rendered ledger rows instead of assuming 1/2/... Waits until the
// expected number of rows is present (POSTs update the table asynchronously).
async function eventIds(page, expected) {
  const rows = page.locator('tbody tr[data-test^="event-row-"]')
  await expect(rows.first()).toBeVisible()
  if (expected != null) {
    await expect.poll(async () => rows.count()).toBe(expected)
  }
  const n = await rows.count()
  const ids = []
  for (let i = 0; i < n; i++) {
    const dt = await rows.nth(i).getAttribute('data-test')
    ids.push(Number(dt.replace('event-row-', '')))
  }
  return ids
}

test('误归还导致报废后撤销：恢复柜外与原累计，审计可追溯，可再次归还', async ({ page }) => {
  const barcode = nextBarcode('SCRAP')
  await createBatch(page, barcode, 10)
  await takeout(page, '2026-01-01T00:00:05Z')
  await doReturn(page, '2026-01-01T00:00:16Z') // +11 > 10 → 误扫报废
  await expect(page.getByTestId('status-badge')).toHaveText('已报废')
  await expect(page.getByTestId('accumulated')).toHaveText('11 秒')

  const [takeoutId, returnId] = await eventIds(page, 2)

  // 仅最近一条（误归还）旁有撤销入口
  await expect(page.getByTestId('revoke-form')).toContainText(`#${returnId}`)
  await expect(page.getByTestId(`revoke-btn-${returnId}`)).toBeVisible()

  await revoke(page, '2026-01-01T00:00:20Z', '误扫归还，样本实际仍在柜外')

  // 撤销归还：扣回该次暴露、恢复柜外、按恢复后累计重新判定为可用
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')
  await expect(page.getByTestId('status-badge')).toHaveText('可用')
  await expect(page.getByTestId('accumulated')).toHaveText('0 秒')
  await expect(page.getByTestId('remaining')).toHaveText('10 秒')
  await expect(page.getByTestId('last-event')).toContainText('取出 @ 2026-01-01T00:00:05Z')

  // 流水保留记录并展示撤销时刻与原因
  const row = page.getByTestId(`event-row-${returnId}`)
  await expect(row).toContainText('已撤销')
  await expect(row).toContainText('2026-01-01T00:00:20Z')
  await expect(row).toContainText('误扫归还，样本实际仍在柜外')
  // 被撤销行不再提供撤销按钮，表单回退到上一条有效事件（取出）
  await expect(page.getByTestId(`revoke-btn-${returnId}`)).toHaveCount(0)
  await expect(page.getByTestId('revoke-form')).toContainText(`#${takeoutId}`)

  // 可再次正确归还：状态机恢复，操作被接受（该样本实际已在外 20 秒）
  await doReturn(page, '2026-01-01T00:00:25Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page.getByTestId('accumulated')).toHaveText('20 秒')
  await expect(page.getByTestId('events-table')).toContainText('归还')
})

test('未超限的误归还撤销后恢复柜外原累计，再次正确归还保持可用', async ({ page }) => {
  const barcode = nextBarcode('FIX')
  await createBatch(page, barcode, 100)
  await takeout(page, '2026-01-01T00:00:05Z')
  await doReturn(page, '2026-01-01T00:00:16Z') // 误扫：+11，仍可用
  await expect(page.getByTestId('accumulated')).toHaveText('11 秒')

  const [, returnId] = await eventIds(page, 2)
  await revoke(page, '2026-01-01T00:00:20Z', '归还时刻扫错')
  await expect(page.getByTestId(`event-row-${returnId}`)).toContainText('已撤销')
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')
  await expect(page.getByTestId('accumulated')).toHaveText('0 秒')
  await expect(page.getByTestId('status-badge')).toHaveText('可用')

  // 用真实归还时刻再次归还：+20，仍在额度内，保持可用
  await doReturn(page, '2026-01-01T00:00:25Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page.getByTestId('accumulated')).toHaveText('20 秒')
  await expect(page.getByTestId('status-badge')).toHaveText('可用')
  await expect(page.getByTestId('conclusion')).toHaveText('可用')
})

test('误取出撤销：批次回到柜内、累计不变、无有效事件时无撤销入口', async ({ page }) => {
  const barcode = nextBarcode('TAKEOUT')
  await createBatch(page, barcode, 100)
  await takeout(page, '2026-01-01T00:00:05Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')

  const [takeoutId] = await eventIds(page, 1)
  await revoke(page, '2026-01-01T00:00:20Z', '误扫取出')
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page.getByTestId('accumulated')).toHaveText('0 秒')
  await expect(page.getByTestId('last-event')).toHaveText('尚无事件')

  const row = page.getByTestId(`event-row-${takeoutId}`)
  await expect(row).toContainText('已撤销')
  await expect(row).toContainText('误扫取出')
  // 所有事件均已撤销：不再显示撤销表单
  await expect(page.getByTestId('revoke-form')).toHaveCount(0)

  // 可从恢复后的柜内状态继续正确操作
  await takeout(page, '2026-01-01T00:00:30Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')
})

test('非最近事件不提供撤销入口', async ({ page }) => {
  const barcode = nextBarcode('NONLAST')
  await createBatch(page, barcode, 100)
  await takeout(page, '2026-01-01T00:00:05Z')
  await doReturn(page, '2026-01-01T00:00:15Z')

  const [takeoutId, returnId] = await eventIds(page, 2)
  // 取出行不是最近事件，没有撤销按钮；归还行才有
  await expect(page.getByTestId(`revoke-btn-${takeoutId}`)).toHaveCount(0)
  await expect(page.getByTestId(`revoke-btn-${returnId}`)).toBeVisible()
  await expect(page.getByTestId('revoke-form')).toContainText(`#${returnId}`)
})

test('撤销时刻/原因为空或格式错误时前端拦截，早于最后操作被服务端拒绝', async ({ page }) => {
  const barcode = nextBarcode('VALID')
  await createBatch(page, barcode, 100)
  await takeout(page, '2026-01-01T00:00:05Z')

  // 错误时刻格式
  await page.getByTestId('revoke-time').fill('2026-01-01 00:00:20')
  await page.getByTestId('revoke-reason').fill('x')
  await page.getByTestId('revoke-submit').click()
  await expect(page.getByTestId('error-banner')).toContainText('RFC3339')

  // 空原因
  await page.getByTestId('revoke-time').fill('2026-01-01T00:00:20Z')
  await page.getByTestId('revoke-reason').fill('   ')
  await page.getByTestId('revoke-submit').click()
  await expect(page.getByTestId('error-banner')).toContainText('撤销原因')

  // 早于/等于批次最后操作的时刻被服务端拒绝（冲突后重载，状态不变）
  await page.getByTestId('revoke-reason').fill('误扫取出')
  await page.getByTestId('revoke-time').fill('2026-01-01T00:00:05Z')
  await page.getByTestId('revoke-submit').click()
  await expect(page.getByTestId('error-banner')).toContainText('time_not_monotonic')
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')
  await expect(page.getByTestId('accumulated')).toHaveText('0 秒')
})

test('两个工位并发撤销同一事件仅一次成功，失败方重载权威状态', async ({ context, page }) => {
  const barcode = nextBarcode('RACE')
  await createBatch(page, barcode, 100)
  await takeout(page, '2026-01-01T00:00:05Z')
  const [takeoutId] = await eventIds(page, 1)

  // 工位 2 也打开同一批次，看到“柜外”，撤销目标同为最近取出事件
  const page2 = await context.newPage()
  await scan(page2, barcode)
  await expect(page2.getByTestId('state-badge')).toHaveText('柜外')
  await expect(page2.getByTestId('revoke-form')).toContainText(`#${takeoutId}`)

  // 两页填好相同的撤销时刻与原因
  for (const p of [page, page2]) {
    await p.getByTestId('revoke-time').fill('2026-01-01T00:00:20Z')
    await p.getByTestId('revoke-reason').fill('两个工位并发撤销')
  }
  // 同时提交：恰好一个成功；两页最终都收敛到柜内（失败页显示冲突并自动重载）。
  await Promise.all([
    page.getByTestId('revoke-submit').click(),
    page2.getByTestId('revoke-submit').click()
  ])

  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page2.getByTestId('state-badge')).toHaveText('柜内')

  // 至少一个工位收到明确冲突（重复撤销 / 并发状态已变）。
  const banners = await Promise.all([
    page.getByTestId('error-banner').textContent().catch(() => ''),
    page2.getByTestId('error-banner').textContent().catch(() => '')
  ])
  const conflicts = banners.filter((t) => /already_revoked|not_last_event|invalid_transition/.test(t))
  expect(conflicts.length).toBeGreaterThanOrEqual(1)

  // 权威结果：柜内、累计 0、事件仅一条且只被撤销一次
  await page.reload()
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page.getByTestId('accumulated')).toHaveText('0 秒')
  await expect(page.getByTestId(`event-row-${takeoutId}`)).toContainText('已撤销')
  await page2.close()
})

test('不使用撤销能力时原有临界计时流程结果一致', async ({ page }) => {
  const barcode = nextBarcode('BOUNDARY')
  await createBatch(page, barcode, 10)
  await takeout(page, '2026-01-01T00:00:05Z')
  await doReturn(page, '2026-01-01T00:00:15Z') // 恰好 == 上限
  await expect(page.getByTestId('accumulated')).toHaveText('10 秒')
  await expect(page.getByTestId('remaining')).toHaveText('0 秒')
  await expect(page.getByTestId('status-badge')).toHaveText('可用')

  // 未撤销：有效事件不带任何撤销审计字样
  const [takeoutId, returnId] = await eventIds(page, 2)
  await expect(page.getByTestId(`event-row-${takeoutId}`)).not.toContainText('已撤销')
  await expect(page.getByTestId(`event-row-${returnId}`)).not.toContainText('已撤销')
  await page.reload()
  await expect(page.getByTestId('conclusion')).toHaveText('可用')
})
