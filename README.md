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
- 规则版本化（本轮新增）：服务端可在会话中切换动作规则（暂时封闭一片区域、
  改变某类动作的能量消耗），每次切换版本号 +1 并作为 `rules` 事件落盘。
  - 客户端每个输入都携带形成时所依据的 `ruleVersion`；离线期间形成的输入重连后
    仍声明原依据，服务端逐项明确判定：按当前版本接受、拒绝（`sealed` /
    `insufficient-energy` / `blocked`），或对"旧依据 + 新规则禁止"的输入置
    `needsReconfirm`，由用户显式重新确认，绝不静默改写原输入。
  - 拒绝一项后其余输入仍按原 seq 顺序处理，依赖被否决动作的后续输入各自获得
    独立理由；页面展示规则切换前的预测、逐项确认结果与最终状态差异，预测修正
    不会抹掉原规则依据与拒绝记录。
  - 含待确认输入的会话可保存后重开，迟到确认按原规则依据继续结算；规则切换、
    重复送达与部分确认的组合都与权威重放逐 tick 对 hash。
- 事件保存与重放：有输入的 tick 追加为 `tick`，已入站但尚未执行的缺口输入追加为
  `queued`，规则切换追加为 `rules`，文件位于 `server/data/<room>.jsonl`。重放接口纯靠日志重建全部快照并与
  实时记录逐 tick 比对 hash；重启后 `LoadRoom` 先重放已确认 tick，再恢复仍未执行的
  `queued` 输入，tick 编号不会回绕。
- 共享资源裁决（本轮新增）：房间内声明有限共享资源（`pass` 通行名额 ×1、
  `relic` 场景遗物 ×1），多个客户端可离线形成对同一资源的 `claim` 预测。
  - 争用轮（contention round）：`claim` 不直接进 tick 批次，而是进入该资源当前
    开放的争用轮。每轮有可持久化的裁决身份（`<resource>#<n>`，随 tick 记录与
    `round` 日志落盘）、明确的收集边界与稳定顺序。
  - 收集边界：房间内全部角色的 `claim` 到齐 → 下一轮 tick 裁决；或到达收集
    截止（约 2.5s）/ 显式取消（`POST /api/rooms/{id}/rounds/{rid}/cancel`）→
    本轮取消，已收集的 `claim` 以 `round-cancelled` + `needsReconfirm` 拒绝，
    资源不授予任何人。缺少某客户端输入导致本轮无法封闭时，界面明确显示
    "等待某角色的输入"，绝不按网络到达先后默默决定。
  - 裁决顺序稳定：按角色声明顺序再按 seq，与消息到达/分批方式无关——同样两个
    动作合成一批或分成多批到达，最终归属与拒绝依据完全一致。胜者授予资源并
    写入快照（参与 hash），负者以 `resource-exhausted` 拒绝；同一角色重复领取
    以 `already-claimed` 拒绝。
  - 未确定不提前显示成功：仍收集在开放轮中的 `claim` 即使同会话更靠后的 seq
    已被确认，也保持"裁决中"待确认状态，直到本轮封闭由权威结算。
  - 一个共享资源的使用不会被重复送达或迟到确认重复授予：stale seq 的重复
    `claim` 直接幂等返回，不会再次执行；重启重放后持有者不变。
  - 动作依赖：输入可声明 `dependsOn`（同会话先前 seq）。前置动作被跨客户端
    冲突拒绝后，依赖它的后续输入以独立原因 `dependency-failed` 拒绝，互不依赖的
    动作照常按 seq 处理。
  - 规则可临时关闭资源领取（`claimsClosed`）：离线期间按旧依据形成的 `claim`
    重连后保留原 `ruleVersion`，被判定 `sealed` + `needsReconfirm`，由用户显式
    重新确认，绝不静默改写。
  - 页面并排展示各客户端原预测（"本地预测领取待确认"）、权威裁决（持有者、
    剩余数量、逐项拒绝原因）与修正后状态；历史拒绝记录不随最终画面一致而消失。
    含未确认 `claim` 的会话可保存重开，乱序/迟到输入按原依据继续结算，重放
    校验覆盖争用、依赖拒绝与规则切换的组合。
- 房间恢复：页面把 roomId 与各客户端视图（待确认队列、确认水位、预测层、
  拒绝回看、争用轮状态）写入 localStorage；刷新后按保存的 roomId 重连原房间
  与原会话（服务重启时由日志重放恢复，含未封闭的争用轮），继续核对旧预测，
  不会另开新房间。需要全新房间时可用页头"新建房间"显式放弃。

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
- `POST /api/rooms/{id}/inputs`：`{ sessionId, charId, seq, cmd, ruleVersion }`，
  返回 `202 accepted` / `200 stale` / `403 forbidden`
- `POST /api/rooms/{id}/rules`：`{ sealed: [[x,y]...], costs: {up:1,...} }`，
  切换新版本规则（下一 tick 生效并落盘），SSE 快照携带当前 `rules` 与逐项判定理由
- `POST /api/rooms/{id}/rounds/{rid}/cancel`：显式取消一个开放中的争用轮，
  已收集的 `claim` 以 `round-cancelled` 拒绝，资源不授予
- `POST /api/rooms/{id}/replay`：从落盘事件重放并返回 hash 比对结果
- `GET  /api/rooms/{id}/journal`：原始 JSONL

## 测试

```bash
cd server && go test ./...          # 幂等、防接管、确定性、重放恢复、SSE 回填
cd frontend && npx vitest run       # 预测→确认→偏差修正状态机
```
