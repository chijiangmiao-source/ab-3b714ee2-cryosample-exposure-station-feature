# 冷冻库样本扫码计时站

多人轮班共用冷冻库时，纸面计时容易把同一批样本的重复归还算成两段暴露，甚至让已超限的批次再次出库。本站用扫码 + 服务端状态机替代纸面记录：

- **Vue 3 页面**：扫描/输入批次条码，查询状态，提交「取出 / 归还」，展示柜内外状态、累计与剩余秒数、最后事件与可用结论；刷新后状态不变（服务端持久化）。误扫的最近一条事件可在页面上撤销（需填写撤销时刻与原因），流水保留审计信息。
- **Go 1.25 + Gin + SQLite API**：持久化批次与事件，在单事务内完成校验与写入；并发下同一旧状态最多成功一次。撤销同样在单事务内写入审计并反向更新批次聚合。
- **测试**：testify（API 转换边界与并发）、Vitest（前端工具与组件）、Playwright（端到端临界场景）。
- **交付**：Docker Compose 发布前端与 API，宿主端口由 `WEB_PORT` / `API_PORT` 覆盖；`verify` 为一次性验收服务。

## 时间格式

所有时刻（`createdAt`、事件 `at`）统一为 **带 `Z` 的 RFC3339 整秒**，例如 `2026-09-13T08:00:00Z`：

- 必须是 UTC `Z` 后缀，不接受 `+08:00` 等偏移；
- 必须整秒，不接受 `.5Z` 等小数秒；
- 格式非法 → `400 invalid_time`；
- 每个事件时刻必须**严格晚于**该批次上一时刻（创建时刻或上一事件时刻），否则 → `409 time_not_monotonic`。

## 状态机与计时规则

- 批次创建时：**柜内（in）**、累计暴露 `0`、状态 **可用（usable）**。
- 只允许交替转换：`柜内 → 取出 → 柜外 → 归还 → 柜内`。
- 归还时累计增加 `归还时刻 − 对应取出时刻` 的秒数。
- 累计 **≤** 允许暴露秒数：可用（等于上限仍可用）；累计 **>** 上限：**永久报废（scrapped）**。
- 报废批次不得再取出（`409 batch_scrapped`）。
- 连续取出、无取出归还、倒序时间 → `409`，且事务内不写事件、不改累计。
- 并发提交同一旧状态（如两个工位同时归还）最多一个成功，暴露只计一次。

## 撤销误扫事件

值班员发现刚完成的取出/归还是误扫时，可撤销**当前最后一条未撤销事件**（更早的记录需先逐条撤销其后的记录）：

- 撤销需填写撤销时刻（同样是带 `Z` 的整秒，不得早于批次最后操作时刻）与非空原因，二者随事件留档可追溯。
- **撤销取出**：批次回到柜内，累计不变。
- **撤销归还**：扣回该次暴露、恢复柜外状态，并按恢复后的累计重新判定可用/报废（误报废可因此恢复可用）。
- 撤销在单个事务内写入 `undone_at` / `undo_reason` 并反向更新批次聚合；被撤销的事件**保留在流水中**（带审计信息），但不再参与状态判定；批次的 `lastEvent` 回退到最近一条未撤销事件。
- 非最近事件（`409 not_last_event`）、重复撤销（`409 already_undone`）、撤销时刻早于批次最后操作（`409 time_not_monotonic`）、并发状态已变（`409 invalid_transition`）都会整事务回滚：不改累计、不改柜内外状态、不改流水。
- 两个工位并发撤销同一事件，最多一个成功。

## API 接口

Base path：`/api`（前端同源反代；开发时 Vite 代理到 `localhost:8080`）。

### `POST /api/batches` — 创建批次

```json
{ "barcode": "B-001", "allowedSeconds": 3600, "createdAt": "2026-09-13T08:00:00Z" }
```

- `barcode`：非空字符串（≤128 字符），唯一；
- `allowedSeconds`：正整数；
- `createdAt`：创建时刻（格式见上）。
- `201` → 批次 JSON；条码重复 → `409 duplicate_barcode`；参数非法 → `400`。

### `GET /api/batches/:barcode` — 查询批次

`200` →

```json
{
  "barcode": "B-001",
  "allowedSeconds": 3600,
  "state": "in",                 // in=柜内 | out=柜外
  "status": "usable",            // usable=可用 | scrapped=已报废
  "usable": true,
  "accumulatedSeconds": 30,
  "remainingSeconds": 3570,
  "createdAt": "2026-09-13T08:00:00Z",
  "lastEvent": { "id": 2, "type": "return", "at": "2026-09-13T08:05:00Z", "deltaSeconds": 30, "undoneAt": null, "undoReason": null }
}
```

未找到 → `404 not_found`。`lastEvent` 为 `null` 表示尚无（未撤销的）事件；`deltaSeconds` 仅归还事件携带（本次暴露秒数）；`undoneAt` / `undoReason` 为撤销审计字段，未撤销时为 `null`（旧客户端可安全忽略）。

### `POST /api/batches/:barcode/events` — 提交取出/归还

```json
{ "type": "takeout", "at": "2026-09-13T08:04:30Z" }
```

- `type`：`"takeout"`（取出）或 `"return"`（归还）；
- `at`：事件时刻（格式见上，须严格晚于该批次上一时刻）。
- `201` → 事件已写入后的批次 JSON；
- `404 not_found`：条码不存在；
- `409 invalid_transition`：连续取出 / 无取出归还 / 并发状态已变；
- `409 batch_scrapped`：报废批次再取出；
- `409 time_not_monotonic`：事件时刻未严格晚于上一时刻；
- `400 invalid_event_type` / `400 invalid_time`：参数非法。

所有 `409` 均为整事务回滚：不写事件、不改累计。

### `GET /api/batches/:barcode/events` — 事件流水

`200` → `{ "events": [ { "id", "type", "at", "deltaSeconds", "undoneAt", "undoReason" }, ... ] }`（按写入顺序）。被撤销的事件仍保留在流水中，`undoneAt` / `undoReason` 记录撤销时刻与原因。

### `POST /api/batches/:barcode/events/:id/undo` — 撤销误扫事件

```json
{ "at": "2026-09-13T08:10:00Z", "reason": "误扫归还" }
```

- `:id`：要撤销的事件 id；只允许撤销当前最后一条未撤销事件；
- `at`：撤销时刻（格式同上，不得早于批次最后操作时刻）；
- `reason`：非空原因（留档可追溯）。
- `200` → 撤销后的批次 JSON；
- `404 not_found`：批次或事件不存在；
- `409 not_last_event`：不是最后一条未撤销事件；
- `409 already_undone`：该事件已被撤销；
- `409 time_not_monotonic`：撤销时刻早于批次最后操作时刻；
- `409 invalid_transition`：并发导致批次状态已变；
- `400 invalid_event_id` / `400 invalid_time` / `400 reason_required`：参数非法。

所有 `409` 均为整事务回滚：不写撤销、不改累计与状态。

### `GET /api/health` — 健康检查

`200` → `{ "ok": true }`。

### 错误格式

```json
{ "error": { "code": "invalid_transition", "message": "batch is already out of the cabinet" } }
```

## 运行（Docker Compose）

```bash
# 构建并启动前端 + API（默认宿主端口：web 8081，api 8080）
docker compose up --build -d api web

# 自定义宿主端口
WEB_PORT=9000 API_PORT=9001 docker compose up --build -d api web

# 一次性验收服务：跑完全部临界/并发检查后退出，退出码即结果
docker compose up --build --exit-code-from verify verify

# 停止与清理（数据在命名卷 api-data 中）
docker compose down          # 保留数据
docker compose down -v       # 连同数据一起删除
```

| 服务    | 说明                                   | 宿主端口（默认） |
| ------- | -------------------------------------- | ---------------- |
| `web`   | nginx 托管前端并反代 `/api` → `api`    | `WEB_PORT`=8081  |
| `api`   | Go 1.25 + Gin + SQLite（卷 `/data`）   | `API_PORT`=8080  |
| `verify`| 一次性验收（见下）                     | —                |

打开 `http://localhost:8081/`（或自定义 `WEB_PORT`）即可使用。

### verify 验收服务

`verify` 针对运行中的整套服务执行 39 项验收并打印 `PASS/FAIL`，覆盖：创建与初始状态、重复条码 409、时间不单调 409、无取出归还 409、连续取出 409、**临界归还（累计==上限）仍可用**、**超限归还立即报废**、报废后取出 409、**重复归还不二次计时**、**8 路并发取出/归还仅一个成功且暴露只计一次**、拒绝的事件不落库、最终状态可重读（刷新一致）、前端页面可访问，以及撤销能力：**误归还导致报废后撤销并恢复柜外与原累计、可再次正确归还**、误取出撤销、撤销审计留档（时刻+原因）、非最近记录 409、重复撤销 409、撤销时刻早于最后操作 409、空原因/非法时刻 400、未知批次/事件 404、**8 路并发撤销仅一个成功且暴露只扣回一次**、被拒绝的撤销不落库。全部通过以退出码 `0` 结束，否则非零。

## 测试

```bash
# API：testify（转换边界、并发、持久化），含 -race
cd api && go test ./... -count=1
cd api && go test ./... -race

# 前端：Vitest（时间格式、派生逻辑、组件、App 流程）
cd web && npm install && npm test

# 端到端：Playwright（先启动整套服务，默认打 WEB_PORT=8081）
cd e2e && npm install && npx playwright install chromium
WEB_PORT=8081 npx playwright test
# 或对本地 dev server：WEB_BASE_URL=http://localhost:5173 npx playwright test
```

## 本地开发

```bash
# API（默认 :8080，DB_PATH 默认 data/app.db）
cd api && go run ./cmd/server

# 前端（:5173，/api 代理到 :8080）
cd web && npm install && npm run dev
```

## 目录结构

```
├── docker-compose.yml      # web / api / verify 三服务
├── api/                    # Go 1.25 + Gin + SQLite
│   ├── cmd/server/         # 入口（PORT、DB_PATH 环境变量）
│   └── internal/
│       ├── app/            # 路由与处理器（+ testify 测试）
│       └── store/          # SQLite 事务化状态机
├── web/                    # Vue 3 + Vite（nginx 反代 /api）
│   ├── src/                # App.vue、BatchCard、api.js、lib/
│   └── tests/              # Vitest
├── e2e/                    # Playwright 端到端
└── verify/                 # 一次性验收服务（Go，无第三方依赖）
```
