import { describe, expect, it } from "vitest";
import { advance, applyImpulse, applyInput, newState, replay, StoredEvent, MILLI } from "./sim";

describe("deterministic kernel", () => {
  it("same inputs produce same state", () => {
    const a = newState(["alpha", "beta"]);
    const b = newState(["alpha", "beta"]);
    for (let i = 1; i <= 30; i++) {
      applyInput(a, "alpha", i, 1, 0);
      applyInput(b, "alpha", i, 1, 0);
      advance(a);
      advance(b);
    }
    expect(JSON.stringify(a)).toBe(JSON.stringify(b));
  });

  it("stale/duplicate input sequence never applies twice", () => {
    const s = newState(["alpha"]);
    applyInput(s, "alpha", 1, 1, 0);
    applyInput(s, "alpha", 1, 1, 0);
    applyInput(s, "alpha", 0, 1, 0);
    expect(s.chars.alpha.lastSeq).toBe(1);
    expect(s.chars.alpha.vx).toBe(3 * MILLI);
  });

  it("replays the event stream to the same final state", () => {
    const live = newState(["alpha", "beta"]);
    const events: StoredEvent[] = [
      { id: 1, kind: "room", atTick: 0, data: { roomId: "r", charIds: ["alpha", "beta"] } },
    ];
    let id = 1;
    const pushInput = (tick: number, charId: string, seq: number, dirX: number, dirY: number) => {
      events.push({ id: ++id, kind: "input", atTick: tick, data: { charId, seq, dirX, dirY } });
      applyInput(live, charId, seq, dirX, dirY);
    };
    for (let t = 1; t <= 12; t++) {
      pushInput(t, "alpha", t, 1, 0);
      pushInput(t, "beta", t, 0, 1);
      if (t === 6) {
        events.push({ id: ++id, kind: "impulse", atTick: t, data: { charId: "alpha", dirX: -1, dirY: 0 } });
        applyImpulse(live, "alpha", -1, 0);
      }
      events.push({ id: ++id, kind: "checkpoint", atTick: t, data: { hash: "x" } });
      advance(live);
    }
    const frames = replay(events);
    const last = frames[frames.length - 1].state;
    expect(last.tick).toBe(live.tick);
    expect(JSON.stringify(last.chars)).toBe(JSON.stringify(live.chars));
  });
});
