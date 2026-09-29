// Simulated unreliable transport sitting on top of real HTTP/SSE. Every
// request can be delayed, dropped, duplicated, or blocked during a brief
// disconnect - exactly what the authority protocol must survive.

export interface ChaosConfig {
  latencyMs: number;
  jitterMs: number;
  dropRate: number; // 0..1 of requests silently lost
  duplicateRate: number; // 0..1 of requests delivered twice
  disconnected: boolean;
}

export const DEFAULT_CHAOS: ChaosConfig = {
  latencyMs: 80,
  jitterMs: 60,
  dropRate: 0,
  duplicateRate: 0,
  disconnected: false,
};

const delay = (ms: number) => new Promise<void>((res) => setTimeout(res, Math.max(0, ms)));

export class ChaosNet {
  cfg: ChaosConfig;
  counters = { sent: 0, delivered: 0, duplicated: 0, dropped: 0, retried: 0 };

  constructor(cfg: ChaosConfig = { ...DEFAULT_CHAOS }) {
    this.cfg = cfg;
  }

  private roll() {
    return Math.random();
  }

  // send performs an HTTP POST with chaos. When the request is dropped it
  // rejects with a Drop so the caller can decide whether to retransmit.
  async post(path: string, token: string, body: unknown): Promise<Response> {
    this.counters.sent++;
    if (this.cfg.disconnected) throw new Drop("disconnected");
    const wait = this.cfg.latencyMs + Math.random() * this.cfg.jitterMs;
    if (this.roll() < this.cfg.dropRate) {
      this.counters.dropped++;
      await delay(wait * 2); // late loss: wait then fail
      throw new Drop("simulated loss");
    }
    const doFetch = async (): Promise<Response> => {
      await delay(wait + Math.random() * this.cfg.jitterMs);
      if (this.cfg.disconnected) throw new Drop("disconnected");
      const res = await fetch(path, {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-Session-Token": token },
        body: JSON.stringify(body),
      });
      return res;
    };
    if (this.roll() < this.cfg.duplicateRate) {
      this.counters.duplicated++;
      // Fire the duplicate; the caller keeps the first response. The server
      // dedupes by sequence number, so this must never move the player twice.
      void doFetch().catch(() => undefined);
    }
    this.counters.delivered++;
    return doFetch();
  }

  // postWithRetry retransmits on loss/disconnect using the SAME sequence
  // number. Retransmission is how a brief disconnect resumes cleanly.
  async postWithRetry(path: string, token: string, body: { seq?: number }, attempts = 6): Promise<Response> {
    let lastErr: unknown;
    for (let i = 0; i < attempts; i++) {
      try {
        return await this.post(path, token, body);
      } catch (e) {
        lastErr = e;
        this.counters.retried++;
        await delay(250 + i * 200);
      }
    }
    throw lastErr;
  }
}

export class Drop extends Error {}
