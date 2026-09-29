import { ChaosConfig } from "../lib/net";

interface Props {
  chaos: ChaosConfig;
  setChaos: (c: ChaosConfig) => void;
  onKick: (target: string, dx: number, dy: number) => void;
}

export function ChaosPanel({ chaos, setChaos, onKick }: Props) {
  const set = (patch: Partial<ChaosConfig>) => setChaos({ ...chaos, ...patch });
  return (
    <section className="chaos">
      <h2>网络混沌 & 权威扰动</h2>
      <div className="chaos-grid">
        <label>
          基础延迟 {chaos.latencyMs}ms
          <input type="range" min={0} max={500} step={20} value={chaos.latencyMs}
            onChange={(e) => set({ latencyMs: Number(e.target.value) })} />
        </label>
        <label>
          抖动 +{chaos.jitterMs}ms
          <input type="range" min={0} max={400} step={20} value={chaos.jitterMs}
            onChange={(e) => set({ jitterMs: Number(e.target.value) })} />
        </label>
        <label>
          丢包率 {(chaos.dropRate * 100).toFixed(0)}%
          <input type="range" min={0} max={0.9} step={0.05} value={chaos.dropRate}
            onChange={(e) => set({ dropRate: Number(e.target.value) })} />
        </label>
        <label>
          重复率 {(chaos.duplicateRate * 100).toFixed(0)}%
          <input type="range" min={0} max={0.9} step={0.05} value={chaos.duplicateRate}
            onChange={(e) => set({ duplicateRate: Number(e.target.value) })} />
        </label>
      </div>
      <div className="chaos-actions">
        <button className={chaos.disconnected ? "danger" : ""} onClick={() => set({ disconnected: !chaos.disconnected })}>
          {chaos.disconnected ? "恢复连接" : "模拟短暂断线"}
        </button>
        <span className="divider" />
        <span className="kick-label">服务端权威冲击（客户端无法预测）：</span>
        <button onClick={() => onKick("alpha", -1, 0)}>推 alpha ←</button>
        <button onClick={() => onKick("alpha", 1, 0)}>推 alpha →</button>
        <button onClick={() => onKick("beta", 0, -1)}>推 beta ↑</button>
        <button onClick={() => onKick("beta", 0, 1)}>推 beta ↓</button>
      </div>
    </section>
  );
}
