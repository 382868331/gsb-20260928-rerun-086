import { describe, expect, it } from "vitest";
import { ClientEngine, ServerFrame } from "./clientEngine";
import { advance, applyImpulse, applyInput, newState } from "./sim";

function frameOf(tick: number, s: ReturnType<typeof newState>): ServerFrame {
  return { tick, hash: "h" + tick, chars: JSON.parse(JSON.stringify(s.chars)) };
}

describe("client prediction and reconciliation", () => {
  it("predicts movement locally before the server confirms", () => {
    const eng = new ClientEngine("alpha", ["alpha", "beta"]);
    expect(eng.render.chars.alpha.x).toBe(-2000);
    eng.queueInput(1, 0);
    // predicted immediately, no server frame yet
    expect(eng.render.chars.alpha.x).toBe(1000); // -2000 + one 3000 step
    expect(eng.render.chars.alpha.vx).toBe(2400); // post-friction residual
    expect(eng.stats.inFlight).toBe(1);
  });

  it("clears prediction without correction when server agrees", () => {
    const eng = new ClientEngine("alpha", ["alpha", "beta"]);
    eng.queueInput(1, 0);

    // Simulate the exact authoritative result for that one input.
    const server = newState(["alpha", "beta"]);
    applyInput(server, "alpha", 1, 1, 0);
    advance(server);

    const { corrected } = eng.onFrame(frameOf(1, server));
    expect(corrected).toBe(false);
    expect(eng.stats.corrections).toBe(0);
    expect(eng.stats.inFlight).toBe(0);
    expect(eng.render.chars.alpha.x).toBe(server.chars.alpha.x);
  });

  it("snaps to authority when a server impulse breaks the prediction", () => {
    const eng = new ClientEngine("alpha", ["alpha", "beta"]);
    eng.queueInput(1, 0);
    const predicted = eng.render.chars.alpha.x;

    // Server applied the SAME input plus an authoritative kick the client
    // could not know about; final position must replace the prediction.
    const server = newState(["alpha", "beta"]);
    applyInput(server, "alpha", 1, 1, 0);
    applyInput(server, "alpha", 1, 1, 0); // duplicate is ignored by kernel too
    applyImpulse(server, "alpha", -1, 0);
    advance(server);

    const { corrected, delta } = eng.onFrame(frameOf(1, server));
    expect(corrected).toBe(true);
    expect(delta).toBeGreaterThan(0);
    expect(eng.stats.corrections).toBe(1);
    expect(eng.render.chars.alpha.x).toBe(server.chars.alpha.x);
    expect(eng.render.chars.alpha.x).not.toBe(predicted);
  });

  it("duplicate retransmission of the same seq acks once", () => {
    const eng = new ClientEngine("alpha", ["alpha", "beta"]);
    eng.queueInput(1, 0);
    const dup = eng.pending[0];
    eng.markSent(dup.seq);
    eng.markSent(dup.seq); // retransmitted
    expect(dup.sentCount).toBe(2);

    const server = newState(["alpha", "beta"]);
    applyInput(server, "alpha", 1, 1, 0);
    advance(server);
    eng.onFrame(frameOf(1, server));
    expect(eng.pending).toHaveLength(0);
  });
});
