# 游戏服务研发平台 · 虚拟角色状态同步（首轮）

首轮只做房间、角色、会话与**权威状态同步**：Go 服务是唯一权威，React 页面并排
展示两个本地客户端。不包含经典小游戏玩法，后续玩法在此基础上叠加。

## 能力对照

- 权威服务：固定 100ms tick 的确定性移动模拟（边界、墙体、角色间碰撞），
  房间/角色/会话在创建时绑定。
- 客户端预测：按键后本地立刻按相同规则推算位置并渲染（虚线描边），
  收到服务端快照后以快照为准（实心）；预测与确认在画面上有明确区别。
- 服务确认：每个会话的输入带单调递增 `seq`；服务端推进 `appliedSeq` 并在
  SSE 快照里带回。被墙体/碰撞挡住的移动也会消费该 seq（确认但不移动）。
- 偏差修正：客户端把待确认队列在新快照上重新推算；权威拒绝/阻挡某个输入时保留
  该输入的预测点、最终点和原因，后续未拒绝输入仍按原 seq 顺序重放。预测位置与
  权威位置不一致时**直接吸附到服务端坐标并闪烁提示**，不做插值平滑。
- 幂等与旧输入：`seq <= 已确认` 或同一 tick 内重复入队都返回 `stale` 幂等响应，
  旧输入不会执行第二遍。
- 会话安全：输入必须携带创建房间时下发的 session id，服务端校验
  `char.OwnerSession == sessionId`；重连不会接管另一个角色，面板内置“尝试接管对方”。
- 网络异常模拟（纯客户端制造，可观察）：
  - 消息迟到：输入延迟 0–1500ms 发出；
  - 重复送达：按概率把同一输入再投递一次，可看到服务端的 stale ack；
  - 短暂断开：断开 SSE 并串行挂起 POST，离线期间可继续形成本地输入；恢复后按本地
    seq 顺序发送，乱序到达时服务端先缓冲缺口，连续后再进入权威 tick。
  - 保存恢复：待确认输入、最后确认水位、预测层和拒绝回看写入浏览器 localStorage；
    页面中途刷新后会重发待确认输入并从最后权威 tick 续传。
- 事件保存与重放：有输入的 tick 追加为 `tick`，已入站但尚未执行的缺口输入追加为
  `queued`，文件位于 `server/data/<room>.jsonl`。重放接口纯靠日志重建全部快照并与
  实时记录逐 tick 比对 hash；重启后 `LoadRoom` 先重放已确认 tick，再恢复仍未执行的
  `queued` 输入，tick 编号不会回绕。
- 会话内规则切换：权威规则包含版本、临时封闭格和动作能量消耗；切换事件以 `rule`
  追加日志。离线输入保留 `ruleVersion` 原始依据，服务端按当前规则逐项返回
  `accepted`、`rejected` 或 `needs_confirmation`，不会改写客户端原输入。
- 迟到确认：待确认输入拥有独立水位 `decidedSeq`；重复送达仍是 stale。后继输入不被
  阻塞，按原 seq 独立处理；用户重开页面后待确认项从 localStorage 恢复，迟到确认走
  `/confirmations` 并形成新的权威 tick。权威重放会重放规则、拒绝、确认和最终状态。

## 目录

- `server/cmd/server`：HTTP 服务入口（仅标准库，SSE + JSON）。
- `server/internal/game`：确定性引擎、重放、hash。
- `server/internal/platform`：房间/会话、输入幂等、tick 循环、SSE、重放接口。
- `server/internal/store`：JSONL 追加日志。
- `frontend/src`：React 双客户端、预测/修正状态机、混沌传输层。

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
- `GET  /api/rooms/{id}/events`：SSE，支持 `Last-Event-ID` / `?after=N`，以及跨重启的 `?afterTick=N`
- `POST /api/rooms/{id}/inputs`：`{ sessionId, charId, seq, cmd }`，
- `POST /api/rooms/{id}/inputs`：`{ sessionId, charId, seq, cmd, ruleVersion }`，
  返回 `202 accepted` / `200 stale` / `403 forbidden`；裁定细节在后续 SSE 的
  `applied[].decision/reason` 与 `rejected[]` 中给出
- `POST /api/rooms/{id}/rules`：`{ version: 1 | 2 }`，在服务端切换权威规则
- `POST /api/rooms/{id}/confirmations`：迟到用户确认，body 与原输入一致并带原
  `ruleVersion`；服务端按当前规则重新裁定并追加确认 tick
- `POST /api/rooms/{id}/replay`：从落盘事件重放并返回 hash 比对结果
- `GET  /api/rooms/{id}/journal`：原始 JSONL

## 测试

```bash
cd server && go test ./...          # 幂等、防接管、确定性、重放恢复、SSE 回填
cd frontend && npx vitest run       # 预测→确认→偏差修正状态机
```
