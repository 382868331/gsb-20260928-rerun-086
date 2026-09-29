import { useEffect, useMemo, useRef, useState } from "react";
import { FrameDTO } from "../types";
import { ClientEngine, PendingInput } from "../lib/clientEngine";
import { ChaosConfig, ChaosNet, Drop } from "../lib/net";
import { FIELD_HALF, MILLI } from "../lib/sim";
import { joinRoom } from "../lib/api";

interface Props {
  title: string;
  color: string;
  roomId: string;
  token: string;
  chaos: ChaosConfig;
  onSnapshot?: (f: FrameDTO) => void;
}

interface LogLine {
  t: string;
  text: string;
  kind: "in" | "out" | "warn" | "ok";
}

export function ClientPanel({ title, color, roomId, token, chaos, onSnapshot }: Props) {
  const engRef = useRef<ClientEngine | null>(null);
  const netRef = useRef<ChaosNet>(new ChaosNet(chaos));
  const [, force] = useState(0);
  const [charId, setCharId] = useState<string>("");
  const [error, setError] = useState("");
  const [logs, setLogs] = useState<LogLine[]>([]);
  const [flashCorrection, setFlashCorrection] = useState(false);
  const keysRef = useRef<Record<string, boolean>>({});
  const lastInputAt = useRef(0);

  const log = (text: string, kind: LogLine["kind"] = "in") =>
    setLogs((l) => [...l.slice(-40), { t: new Date().toLocaleTimeString(), text, kind }]);

  // keep chaos config synced
  useEffect(() => {
    netRef.current.cfg = chaos;
    engRef.current?.setConnected(!chaos.disconnected);
    if (chaos.disconnected) log("模拟断线：发送被阻断，SSE 暂停", "warn");
  }, [chaos]);

  // join / resume
  useEffect(() => {
    let alive = true;
    joinRoom(roomId, token)
      .then((j) => {
        if (!alive) return;
        setCharId(j.charId);
        const eng = new ClientEngine(j.charId, j.chars);
        eng.loadServerFrame(j.snapshot);
        engRef.current = eng;
        log(`会话绑定角色 ${j.charId}（断线重连只能拿回自己）`, "ok");
        force((n) => n + 1);
      })
      .catch((e) => alive && setError(String(e)));
    return () => {
      alive = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [roomId, token]);

  // SSE stream with Last-Event-ID resume
  useEffect(() => {
    if (!charId) return;
    let es: EventSource | null = null;
    let stopped = false;
    const connect = (lastId: number) => {
      if (stopped) return;
      const url = `/api/rooms/${roomId}/stream?token=${encodeURIComponent(token)}&lastEventId=${lastId}`;
      es = new EventSource(url);
      es.onopen = () => {
        engRef.current?.setConnected(true);
        force((n) => n + 1);
      };
      es.onmessage = (ev) => {
        const msg = JSON.parse(ev.data);
        if (msg.type === "event" && msg.event.id) lastId = msg.event.id;
        if (msg.type === "frame") {
          const eng = engRef.current;
          if (!eng) return;
          const { corrected, delta } = eng.onFrame(msg.frame as FrameDTO);
          onSnapshot?.(msg.frame as FrameDTO);
          if (corrected) {
            log(`权威修正：位置偏差 ${(delta / MILLI).toFixed(3)} 单位，已服从服务端`, "warn");
            setFlashCorrection(true);
            setTimeout(() => setFlashCorrection(false), 350);
          }
          force((n) => n + 1);
        }
      };
      es.onerror = () => {
        engRef.current?.setConnected(false);
        es?.close();
        log("SSE 断开，1.2s 后带 Last-Event-ID 重连补帧", "warn");
        setTimeout(() => connect(lastId), 1200);
        force((n) => n + 1);
      };
    };
    connect(0);
    return () => {
      stopped = true;
      es?.close();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [charId, roomId, token]);

  const sendInput = (dirX: number, dirY: number) => {
    const eng = engRef.current;
    const net = netRef.current;
    if (!eng) return;
    const input: PendingInput = eng.queueInput(dirX, dirY);
    force((n) => n + 1);
    const transmit = async (attempt: number) => {
      try {
        const res = await net.post(`/api/rooms/${roomId}/inputs`, token, {
          seq: input.seq,
          dirX: input.dirX,
          dirY: input.dirY,
          charId: "", // server ignores this and binds to the token owner
        });
        if (res.ok) {
          if (attempt > 0) log(`输入 #${input.seq} 第 ${attempt + 1} 次送达（重传成功）`, "ok");
        } else {
          log(`输入 #${input.seq} 被拒绝 ${res.status}`, "warn");
        }
      } catch (e) {
        if (e instanceof Drop) {
          log(`输入 #${input.seq} 丢失/断线，保持本地预测，稍后重传`, "warn");
          setTimeout(() => transmit(attempt + 1), 400 + attempt * 300);
        }
      } finally {
        force((n) => n + 1);
      }
    };
    eng.markSent(input.seq);
    void transmit(0);
  };

  // keyboard: WASD/arrows, sends one input per 100ms while held
  useEffect(() => {
    const down = (e: KeyboardEvent) => {
      keysRef.current[e.key.toLowerCase()] = true;
    };
    const up = (e: KeyboardEvent) => {
      keysRef.current[e.key.toLowerCase()] = false;
    };
    window.addEventListener("keydown", down);
    window.addEventListener("keyup", up);
    return () => {
      window.removeEventListener("keydown", down);
      window.removeEventListener("keyup", up);
    };
  }, []);

  useEffect(() => {
    const id = setInterval(() => {
      const k = keysRef.current;
      const dirX = (k["d"] || k["arrowright"] ? 1 : 0) - (k["a"] || k["arrowleft"] ? 1 : 0);
      const dirY = (k["s"] || k["arrowdown"] ? 1 : 0) - (k["w"] || k["arrowup"] ? 1 : 0);
      const now = Date.now();
      if ((dirX !== 0 || dirY !== 0) && now - lastInputAt.current >= 100) {
        lastInputAt.current = now;
        sendInput(dirX, dirY);
      }
    }, 50);
    return () => clearInterval(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [roomId, charId]);

  const eng = engRef.current;
  const counters = netRef.current.counters;
  const render = eng?.render;
  const confirmed = eng?.confirmed;
  const ownPredicted = render && charId ? render.chars[charId] : undefined;
  const ownConfirmed = confirmed && charId ? confirmed.chars[charId] : undefined;

  const fieldSize = useMemo(() => FIELD_HALF * 2, []);
  const px = (v: number) => ((v + FIELD_HALF) / fieldSize) * 100;

  const dpad = (x: number, y: number, label: string) => (
    <button className="dpad-btn" onClick={() => sendInput(x, y)}>
      {label}
    </button>
  );

  return (
    <div className={`client ${flashCorrection ? "correction-flash" : ""}`}>
      <div className="client-head">
        <span className="dot" style={{ background: color }} />
        <strong>{title}</strong>
        <span className={`badge ${eng?.stats.connected ? "ok" : "bad"}`}>
          {eng?.stats.connected ? "已连接" : "断线中"}
        </span>
        <span className="badge">角色 {charId || "…"}</span>
        <span className="badge">tick {render?.tick ?? 0}</span>
      </div>

      {error && <div className="error">{error}</div>}

      <div className="arena">
        {/* confirmed positions (hollow, server authority) */}
        {confirmed &&
          Object.values(confirmed.chars).map((c) => (
            <div
              key={`c-${c.id}`}
              className={`avatar confirmed ${c.id === charId ? "mine" : "other"}`}
              style={{ left: `${px(c.x)}%`, top: `${px(c.y)}%` }}
              title={`确认 ${c.id}`}
            />
          ))}
        {/* predicted ghost only for own character */}
        {ownPredicted && (
          <div
            className="avatar predicted"
            style={{
              left: `${px(ownPredicted.x)}%`,
              top: `${px(ownPredicted.y)}%`,
              background: color,
            }}
            title="本地预测"
          />
        )}
        <div className="legend">
          <span><i className="sw predicted" /> 本地预测（实心）</span>
          <span><i className="sw confirmed mine" /> 服务确认（空心）</span>
        </div>
      </div>

      <div className="dpad">
        <span />{dpad(0, -1, "↑")}<span />
        {dpad(-1, 0, "←")}{dpad(0, 1, "↓")}{dpad(1, 0, "→")}
      </div>

      <div className="metrics">
        <div><label>在途输入</label><b>{eng?.stats.inFlight ?? 0}</b></div>
        <div><label>权威修正次数</label><b className={eng && eng.stats.corrections > 0 ? "warn" : ""}>{eng?.stats.corrections ?? 0}</b></div>
        <div><label>最后偏差</label><b>{((eng?.stats.lastDelta ?? 0) / MILLI).toFixed(3)}</b></div>
        <div><label>已确认 seq</label><b>{ownConfirmed?.lastSeq ?? 0}</b></div>
      </div>
      <div className="netstats">
        发送 {counters.sent} · 送达 {counters.delivered} · 重复 {counters.duplicated} · 丢失 {counters.dropped} · 重传 {counters.retried}
      </div>
      <div className="mlog">
        {logs
          .slice()
          .reverse()
          .map((l, i) => (
            <div key={i} className={`log-${l.kind}`}>
              <span className="lt">{l.t}</span> {l.text}
            </div>
          ))}
      </div>
    </div>
  );
}
