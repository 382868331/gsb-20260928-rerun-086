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
- 共享资源裁决（本轮新增）：房间内有一份有限的共享资源（默认 1 个通行名额，
  `config.passes`），两个客户端用 `claim` 动作争用。
  - 裁决顺序事先确定：同一 tick 内按角色声明顺序、再按 seq；跨 tick 先到先得。
    输入到达的先后与分批方式不影响裁决结果，最后到达的预测不会被写成共同结果。
  - 每个客户端离线时仍可预测"取得名额"，重连后由服务端逐项裁决：快照携带
    `passesLeft` 与每个角色的 `passes`，`applied` 记录标明哪个动作实际取得资源
    （`accepted`）、哪些因冲突被拒绝（`resource-exhausted`）及各自规则依据；
    两个客户端折叠同一份权威结论。
  - 动作依赖：输入可声明 `dependsOn`（同会话先前动作的 seq）。被依赖动作被拒绝
    时，依赖动作以独立原因 `dependency-rejected` 拒绝；不相关的动作照常处理。
  - 幂等：重复送达或迟到确认命中 stale，名额只授予一次；规则切换可关闭领取
    （`claimsClosed`），旧依据的领取动作得到 `claims-closed` + `needsReconfirm`，
    名额池不被触碰。
  - 含多个未确认队列的会话可保存重开，乱序输入与确认逐批到达后裁决顺序不变；
    资源状态计入快照 hash，重放/重启恢复与实时记录逐 tick 一致，历史拒绝记录
    不因最终画面一致而消失。
- 事件保存与重放：有输入的 tick 追加为 `tick`，已入站但尚未执行的缺口输入追加为
  `queued`，规则切换追加为 `rules`，文件位于 `server/data/<room>.jsonl`。重放接口纯靠日志重建全部快照并与
  实时记录逐 tick 比对 hash；重启后 `LoadRoom` 先重放已确认 tick，再恢复仍未执行的
  `queued` 输入，tick 编号不会回绕。

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
- `POST /api/rooms/{id}/inputs`：`{ sessionId, charId, seq, cmd, ruleVersion, dependsOn }`，
  `cmd` 含 `up/down/left/right/wait/claim`；返回 `202 accepted` / `200 stale` / `403 forbidden`
- `POST /api/rooms/{id}/rules`：`{ sealed: [[x,y]...], costs: {up:1,...}, claimsClosed: false }`，
  切换新版本规则（下一 tick 生效并落盘），SSE 快照携带当前 `rules`、`passesLeft`
  与逐项判定理由（含 `resource-exhausted` / `dependency-rejected` / `claims-closed`）
- `POST /api/rooms/{id}/replay`：从落盘事件重放并返回 hash 比对结果
- `GET  /api/rooms/{id}/journal`：原始 JSONL

## 测试

```bash
cd server && go test ./...          # 幂等、防接管、确定性、重放恢复、SSE 回填、共享资源裁决
cd frontend && npx vitest run       # 预测→确认→偏差修正状态机、名额裁决折叠
```
