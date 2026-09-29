import { useEffect, useState } from "react";
import { createRoom } from "./lib/api";
import { ChaosConfig, DEFAULT_CHAOS } from "./lib/net";
import { ClientPanel } from "./components/ClientPanel";
import { ChaosPanel } from "./components/ChaosPanel";
import { ReplayPanel } from "./components/ReplayPanel";

export function App() {
  const [room, setRoom] = useState<{ roomId: string; tokens: Record<string, string> } | null>(null);
  const [starting, setStarting] = useState(false);
  const [err, setErr] = useState("");
  const [chaosA, setChaosA] = useState<ChaosConfig>({ ...DEFAULT_CHAOS });
  const [chaosB, setChaosB] = useState<ChaosConfig>({ ...DEFAULT_CHAOS, duplicateRate: 0.2 });

  const start = async () => {
    setStarting(true);
    setErr("");
    try {
      const r = await createRoom();
      setRoom({ roomId: r.roomId, tokens: r.tokens });
    } catch (e) {
      setErr(String(e));
    } finally {
      setStarting(false);
    }
  };

  useEffect(() => {
    document.title = "虚拟角色状态同步 · Go 权威服务";
  }, []);

  const kick = async (charId: string, dx: number, dy: number) => {
    if (!room) return;
    // Either session token may request a demo impulse; the server remains the
    // sole authority that applies and persists it.
    await fetch(`/api/rooms/${room.roomId}/impulse`, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Session-Token": room.tokens.alpha },
      body: JSON.stringify({ charId, dirX: dx, dirY: dy }),
    });
  };

  return (
    <div className="app">
      <header>
        <h1>虚拟角色状态同步 · 权威服务研发台（首轮）</h1>
        <p className="sub">
          Go 固定 tick 权威服务 · 客户端预测 + 服务确认 + 偏差修正 · 事件溯源重放 · 两个本地客户端同屏
        </p>
        {!room ? (
          <div className="start">
            <button className="primary big" onClick={start} disabled={starting}>
              {starting ? "创建房间中…" : "创建房间并生成两个会话"}
            </button>
            {err && <div className="error">{err}（请先启动 Go 服务：cd server &amp;&amp; go run ./cmd/server）</div>}
          </div>
        ) : (
          <div className="roombar">
            房间 <code>{room.roomId}</code> · 会话 token 已在创建时分别绑定 alpha/beta
          </div>
        )}
      </header>

      {room && (
        <>
          <div className="clients">
            <div className="client-wrap">
              <ChaosPanel chaos={chaosA} setChaos={setChaosA} onKick={kick} />
              <ClientPanel
                title="本地客户端 A"
                color="#4f9dff"
                roomId={room.roomId}
                token={room.tokens.alpha}
                chaos={chaosA}
              />
            </div>
            <div className="client-wrap">
              <ChaosPanel chaos={chaosB} setChaos={setChaosB} onKick={kick} />
              <ClientPanel
                title="本地客户端 B"
                color="#ff7a59"
                roomId={room.roomId}
                token={room.tokens.beta}
                chaos={chaosB}
              />
            </div>
          </div>
          <ReplayPanel roomId={room.roomId} />
        </>
      )}
    </div>
  );
}
