# 游戏服务研发平台 · 虚拟角色状态同步（第二轮：离线输入与重连核对）

Go 服务是唯一权威，React 页面并排展示两个本地客户端。第二轮聚焦客户端
短时离线、延迟、乱序、断开重连混合出现时的输入一致性：本地输入保留先后与
身份，重复/旧输入不改变状态，服务端可**最终拒绝部分离线输入**，客户端回看
预测与最终确认的差异，待确认队列可在中途保存后恢复继续核对。

## 能力对照

- 权威服务：固定 100ms tick 的确定性移动模拟（边界、墙体、角色间碰撞），
  房间/角色/会话在创建时绑定。
- 客户端预测：按键后本地立刻按相同规则推算位置并渲染（虚线描边），
  收到服务端快照后以快照为准（实心）；预测与确认在画面上有明确区别。
- 服务确认：每个会话的输入带单调递增 `seq`；服务端推进 `appliedSeq` 并在
  SSE 快照里带回。被墙体/碰撞挡住的移动也会消费该 seq（确认但不移动）。
- 离线继续形成输入：按键立即进入本地 FIFO 发件箱并预测渲染，断线期间输入照常
  排队；重连后按本地 seq 顺序冲刷，POST 失败自动重试（服务端去重保证安全）。
- 乱序不丢不乱执行：比已确认新、但跳号到达的输入进入跨 tick 缺口缓冲，缺失前缀
  到达后按声明顺序、seq 顺序补执行；缓冲中的重复送达仍是幂等 `stale`。
- 服务端拒绝部分离线输入（确定性，入日志、可重放）：
  - `offline-overflow`：一个会话同一 tick 的连续输入超过 `MaxAdmittedPerTick=4`，
    突发尾部输入被拒绝；
  - `offline-window`：seq 跳过未确认窗口（`MaxPendingGap=8`）且前缀无法补齐时拒绝。
  拒绝是**最终确认**：`appliedSeq` 照样前进，重发即 `stale`，旧输入不会二次改状态。
- 偏差修正与回看：客户端把被拒绝输入保留预测坐标并标记原因，未被拒绝的待确认
  尾部仍按原顺序在新权威快照上重新推算；预测与权威不一致时直接吸附并闪烁，
  不做插值平滑、不覆盖服务端结论。
- 幂等与旧输入：`seq <= appliedSeq`（含已拒绝 seq）或缓冲队列内重复入队都返回
  `stale` 幂等响应，旧输入不会执行第二遍。
- 会话安全：输入必须携带创建房间时下发的 session id，服务端校验
  `char.OwnerSession == sessionId`；重连不会接管另一个角色，面板内置“尝试接管对方”。
- 网络异常模拟（纯客户端制造，可观察）：
  - 消息迟到/乱序：每个输入随机延迟 0–1500ms，乱序由服务端缺口缓冲吸收；
  - 重复送达：按概率把同一输入再投递一次，可看到服务端的 stale ack；
  - 短暂断开：断开 SSE 并挂起 POST，期间输入继续在本地形成；恢复后用
    `Last-Event-ID`/`?after=` 续传快照（含拒绝决定），发件箱按序冲刷。
- 中途保存/恢复：客户端把待确认队列、最后确认位置（`appliedSeq`）、预测层、
  下一 seq 持久化到 localStorage（按 room+session 隔离）；刷新页面后从保存恢复，
  并与实时权威流继续核对，可手动清除保存。
- 事件保存与重放：每个有输入的 tick 追加到 `server/data/<room>.jsonl`
  （`init` + `tick` 记录，`tick` 同时含 `inputs` 与 `refused`，附规范快照 hash）。
  重放接口纯靠日志重建全部快照，逐 tick 比对 applied/refused 列表与 hash；
  重启后 `LoadRoom` 用重放恢复到最终 tick 再继续，tick 编号不会回绕。

## 目录

- `server/cmd/server`：HTTP 服务入口（仅标准库，SSE + JSON）。
- `server/internal/game`：确定性引擎、重放、hash。
- `server/internal/platform`：房间/会话、输入幂等、tick 循环、SSE、重放接口。
- `server/internal/store`：JSONL 追加日志。
- `frontend/src`：React 双客户端、预测/修正/拒绝回看状态机、FIFO 混沌传输层、
  localStorage 保存恢复（`storage.ts`）。

## 运行

```bash
# 终端 1：权威服务（默认 :8080，日志目录 ./data）
cd server && go run ./cmd/server -addr :8080 -data ./data

# 终端 2：前端（/api 已代理到 :8080）
cd frontend && npm run dev
```

打开 http://localhost:5173 ，页面自动建一个两角色房间并并排显示客户端 A/B。

## HTTP

- `POST /api/rooms` → `{ roomId, config, chars, sessions }`（sessions 即两个角色的会话凭据）
- `GET  /api/rooms/{id}/events`：SSE，支持 `Last-Event-ID` / `?after=N` 断线续传
- `POST /api/rooms/{id}/inputs`：`{ sessionId, charId, seq, cmd }`，
  返回 `202 accepted` / `200 stale` / `403 forbidden`
- `POST /api/rooms/{id}/replay`：从落盘事件重放并返回 hash 比对结果
- `GET  /api/rooms/{id}/journal`：原始 JSONL

## 测试

```bash
cd server && go test ./...          # 幂等、防接管、确定性、重放恢复、SSE 回填
cd frontend && npx vitest run       # 预测→确认→偏差修正状态机
```
