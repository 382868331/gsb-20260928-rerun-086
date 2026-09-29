# 游戏服务研发平台 · 首轮：虚拟角色状态同步

Go 运行权威服务，React 同屏展示两个本地客户端。首轮只做房间 / 角色 / 会话基础，
完成客户端预测、服务确认与偏差修正；不做经典小游戏，后续玩法在权威框架上扩展。

## 运行

```bash
# 1) 权威服务（:8080，事件日志写到 server/data/*.jsonl）
cd server
go run ./cmd/server            # 可选：-addr :8080 -data ./data

# 2) 前端（:5173，已配置 /api 代理到 8080）
cd frontend
npm install
npm run dev
```

打开 http://localhost:5173 ，点击“创建房间并生成两个会话”，同屏出现客户端 A/B。
用 WASD / 方向键或方向按钮移动（页面需获得焦点）。

## 权威与一致性模型

- **固定 tick（100ms）**：服务端每 tick 最多为每个角色应用一个已排队输入，
  然后推进物理（速度→位移→摩擦→边界）。全部数值用 int 毫单位（1 单位=1000），
  定点运算，避免浮点差异。
- **客户端预测**：输入按客户端自增 `seq` 编号，立即本地推进，同时发送服务端。
- **服务确认与回滚重放**：收到权威帧后，用 `lastSeq` 确认已应用输入，状态整体
  回滚到权威快照，再按序重放未确认输入。预测正确则无修正；有权威冲击等未知事件
  时产生偏差，**直接服从服务端结论并 snap，不做任何掩盖性平滑**。
- **旧输入不执行两遍**：服务端以角色 `lastSeq` + tick 内排队去重；迟到、重复
  重传的相同 `seq` 返回 `deduped` 但只生效一次。
- **会话不可接管**：建房即生成两个随机 token，分别绑定 alpha/beta 并持久化。
  重连用同一 token 只能拿回自己的角色；伪造 token 返回 401，房间满后第三个 token
  无法绑定（409）。请求体里的 `charId` 会被服务端按 token 改写。
- **短暂断线**：HTTP 输入失败按同一 `seq` 重传；SSE 断开后带 `Last-Event-ID`
  重连，服务端补发错过的输入/冲击事件并给当前快照，不丢帧。

## 混沌模拟（每客户端独立可调）

延迟 / 抖动 / 丢包率 / 重复率 / 一键短暂断线，均在前端传输层注入。
“服务端权威冲击”只由服务端产生并持久化，客户端无法预测，用于直观看到修正：
界面上实心圆点是**本地预测**，绿色虚线圈是**服务确认**；修正时面板橙色闪烁，
并累计修正次数和最后偏差。

## 事件溯源与确定性重放

每个房间一个 JSONL 追加日志：`room`、`session`、`input`、`impulse`，以及每 tick 的
`checkpoint`（权威状态 hash）。`GET /api/rooms/{id}/events?replay=1` 仅依据事件流
重建全部帧，并逐 tick 与 checkpoint hash 比对：相同事件必然得到相同结果，
重放末帧 hash 等于日志最后 checkpoint。前端“拉取事件日志并重放”可拖动时间轴
查看每一帧重放坐标。实时循环不依赖重放，画面永远以实时权威帧为准。

## 测试

```bash
cd server && go test ./...       # 确定性、幂等、会话绑定、HTTP 全链路、重放一致
cd frontend && npx vitest run    # 预测、确认、权威修正 snap、事件流重放
```

## 目录

- `server/internal/sim`：确定性定点物理内核（Go）
- `server/internal/store`：JSONL 追加事件日志
- `server/internal/replay`：从事件流重建并校验快照
- `server/internal/engine`：房间/tick 循环、输入幂等、会话绑定、SSE 广播
- `server/cmd/server`：HTTP/SSE 接口
- `frontend/src/lib`：与 Go 同构的 TS 内核、预测/修正引擎、混沌传输
- `frontend/src/components`：双客户端视图、混沌面板、重放面板
