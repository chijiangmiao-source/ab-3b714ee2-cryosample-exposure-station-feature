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

test('临界归还仍可用，刷新后状态保持一致', async ({ page }) => {
  const barcode = nextBarcode('BOUNDARY')
  await createBatch(page, barcode, 10)

  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page.getByTestId('status-badge')).toHaveText('可用')
  await expect(page.getByTestId('accumulated')).toHaveText('0 秒')
  await expect(page.getByTestId('remaining')).toHaveText('10 秒')
  await expect(page.getByTestId('last-event')).toHaveText('尚无事件')

  await takeout(page, '2026-01-01T00:00:05Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')
  await expect(page.getByTestId('takeout-btn')).toBeDisabled()
  await expect(page.getByTestId('return-btn')).toBeEnabled()

  // 恰好等于上限：累计 10 == 允许 10，仍然可用
  await doReturn(page, '2026-01-01T00:00:15Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page.getByTestId('accumulated')).toHaveText('10 秒')
  await expect(page.getByTestId('remaining')).toHaveText('0 秒')
  await expect(page.getByTestId('conclusion')).toHaveText('可用')
  await expect(page.getByTestId('last-event')).toContainText('归还 @ 2026-01-01T00:00:15Z')
  await expect(page.getByTestId('last-event')).toContainText('本次 +10 秒')

  // 刷新后保持一致（状态持久化在服务端）
  await page.reload()
  await expect(page.getByTestId('batch-card')).toBeVisible()
  await expect(page.getByTestId('card-barcode')).toHaveText(barcode)
  await expect(page.getByTestId('accumulated')).toHaveText('10 秒')
  await expect(page.getByTestId('remaining')).toHaveText('0 秒')
  await expect(page.getByTestId('conclusion')).toHaveText('可用')
  await expect(page.getByTestId('events-table')).toContainText('取出')
  await expect(page.getByTestId('events-table')).toContainText('归还')
})

test('超限归还立即报废，报废批次不得再取出', async ({ page }) => {
  const barcode = nextBarcode('SCRAP')
  await createBatch(page, barcode, 10)

  await takeout(page, '2026-01-01T00:00:05Z')
  await doReturn(page, '2026-01-01T00:00:16Z') // +11 秒 > 上限 10

  await expect(page.getByTestId('accumulated')).toHaveText('11 秒')
  await expect(page.getByTestId('status-badge')).toHaveText('已报废')
  await expect(page.getByTestId('conclusion')).toContainText('已报废')
  await expect(page.getByTestId('takeout-btn')).toBeDisabled()
  await expect(page.getByTestId('return-btn')).toBeDisabled()

  await page.reload()
  await expect(page.getByTestId('conclusion')).toContainText('已报废')
})

test('重复/竞态归还不会二次计时', async ({ context, page }) => {
  const barcode = nextBarcode('RACE')
  await createBatch(page, barcode, 100)
  await takeout(page, '2026-01-01T00:00:05Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')

  // 第二个工位同时打开同一批次，看到的也是“柜外”
  const page2 = await context.newPage()
  await scan(page2, barcode)
  await expect(page2.getByTestId('state-badge')).toHaveText('柜外')

  // 工位 1 归还成功
  await doReturn(page, '2026-01-01T00:00:15Z')
  await expect(page.getByTestId('accumulated')).toHaveText('10 秒')
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')

  // 工位 2 仍停留在过期的“柜外”视图，重复归还被 409 拒绝
  await doReturn(page2, '2026-01-01T00:00:20Z')
  await expect(page2.getByTestId('error-banner')).toContainText('invalid_transition')
  // 拒绝后页面重新拉取权威状态：累计仍是 10 秒，没有二次计时
  await expect(page2.getByTestId('accumulated')).toHaveText('10 秒')
  await expect(page2.getByTestId('state-badge')).toHaveText('柜内')

  await page.reload()
  await expect(page.getByTestId('accumulated')).toHaveText('10 秒')
  await page2.reload()
  await expect(page2.getByTestId('accumulated')).toHaveText('10 秒')
  await page2.close()
})

test('事件时间不严格递增时被拒绝并提示', async ({ page }) => {
  const barcode = nextBarcode('ORDER')
  await createBatch(page, barcode, 10)

  // 与创建时刻相同：不是“严格晚于”，409
  await takeout(page, T0)
  await expect(page.getByTestId('error-banner')).toContainText('time_not_monotonic')
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page.getByTestId('accumulated')).toHaveText('0 秒')
})
