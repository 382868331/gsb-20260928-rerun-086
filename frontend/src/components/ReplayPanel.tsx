import { useState } from "react";
import { getReplay } from "../lib/api";
import { replay, StoredEvent } from "../lib/sim";
import { MILLI } from "../lib/sim";

interface Props {
  roomId: string;
}

export function ReplayPanel({ roomId }: Props) {
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<{
    events: StoredEvent[];
    frameIndex: number;
    total: number;
    mismatchTicks: number[];
    liveHash: string;
    loggedHash: string;
    currentHash: string;
    note: string;
  } | null>(null);
  const [error, setError] = useState("");

  const run = async () => {
    setBusy(true);
    setError("");
    try {
      const r = await getReplay(roomId);
      const frames = replay(r.events as StoredEvent[]);
      setResult({
        events: r.events as StoredEvent[],
        frameIndex: frames.length - 1,
        total: frames.length,
        mismatchTicks: r.mismatchTicks,
        liveHash: r.liveHash,
        loggedHash: r.loggedHash,
        currentHash: r.currentHash,
        note:
          r.loggedHash === r.liveHash && r.mismatchTicks.length === 0
            ? "事件日志逐 tick 重放，与记录的权威 checkpoint hash 完全一致"
            : "重放与权威 checkpoint 出现不一致！",
      });
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  };

  const frames = result ? replay(result.events) : [];
  const frame = frames[result?.frameIndex ?? 0];
  const inputEvents = result?.events.filter((e) => e.kind === "input").length ?? 0;
  const impulseEvents = result?.events.filter((e) => e.kind === "impulse").length ?? 0;

  return (
    <section className="replay">
      <h2>事件保存与确定性重放</h2>
      <p className="hint">
        服务端把每次输入、权威扰动与每 tick 的 checkpoint 追加到 JSONL 事件日志；重放只依赖事件流，
        结果由服务端 hash 逐 tick 校验，不使用渲染平滑。
      </p>
      <button className="primary" onClick={run} disabled={busy}>
        {busy ? "重放中…" : "拉取事件日志并重放"}
      </button>
      {error && <div className="error">{error}</div>}
      {result && (
        <div className="replay-body">
          <div className={`replay-verdict ${result.mismatchTicks.length === 0 && result.loggedHash === result.liveHash ? "good" : "bad"}`}>
            {result.note}
            {result.mismatchTicks.length > 0 && (
              <div>不一致 tick：{result.mismatchTicks.join(", ")}</div>
            )}
          </div>
          <div className="replay-stats">
            事件 {result.events.length} 条（输入 {inputEvents} / 权威扰动 {impulseEvents}），重放帧 {result.total} ·
            重放末帧 <code>{result.liveHash}</code> = 日志 checkpoint <code>{result.loggedHash}</code>
          </div>
          <input
            type="range"
            min={0}
            max={result.total - 1}
            value={result.frameIndex}
            onChange={(e) => setResult({ ...result, frameIndex: Number(e.target.value) })}
          />
          {frame && (
            <div className="replay-grid">
              <span>重放 tick {frame.tick}</span>
              {Object.values(frame.state.chars).map((c) => (
                <span key={c.id}>
                  {c.id}: x={(c.x / MILLI).toFixed(3)} y={(c.y / MILLI).toFixed(3)} seq={c.lastSeq}
                </span>
              ))}
            </div>
          )}
        </div>
      )}
    </section>
  );
}
