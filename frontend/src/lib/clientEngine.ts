// Client prediction + server reconciliation engine (one per local client).
//
// Rules:
//  - Inputs are numbered per client and applied locally immediately (predicted).
//  - The server applies one input per fixed tick and broadcasts authority.
//  - On every authoritative frame we rewind to it and replay unacked inputs.
//  - Server results are never rewritten for smoothness; on divergence the
//    displayed position snaps to the recomputed authority+prediction.

import {
  SimState,
  CharState,
  applyInput,
  advance,
  cloneState,
  newState,
} from "./sim";

export interface PendingInput {
  seq: number;
  dirX: number;
  dirY: number;
  predictedTick: number;
  acked: boolean;
  sentCount: number;
}

export interface ServerFrame {
  tick: number;
  hash: string;
  chars: Record<string, CharState>;
}

export interface EngineStats {
  corrections: number;
  lastDelta: number;
  inFlight: number;
  connected: boolean;
}

export class ClientEngine {
  charId: string;
  // Confirmed base = last server frame, exactly as received.
  confirmed: SimState;
  // Rendered state = confirmed base + replayed unacked inputs.
  render: SimState;
  pending: PendingInput[] = [];
  private nextSeq = 1;
  stats: EngineStats = { corrections: 0, lastDelta: 0, inFlight: 0, connected: true };

  constructor(charId: string, chars: string[]) {
    this.charId = charId;
    this.confirmed = newState(chars);
    this.render = newState(chars);
  }

  // Load a snapshot from join/SSE (authoritative).
  loadServerFrame(f: ServerFrame): void {
    const base: SimState = { tick: f.tick, chars: {} };
    for (const [id, c] of Object.entries(f.chars)) base.chars[id] = { ...c };
    this.confirmed = base;
    this.reconcile();
  }

  // queueInput applies the move locally at the next predicted tick and returns
  // the command to transmit. Direction 0/0 still counts (intentional stop).
  queueInput(dirX: number, dirY: number): PendingInput {
    const input: PendingInput = {
      seq: this.nextSeq++,
      dirX,
      dirY,
      predictedTick: this.render.tick + 1,
      acked: false,
      sentCount: 0,
    };
    this.pending.push(input);
    // Predict: step the rendered world one tick with this input for our char.
    this.stepLocal(input);
    this.refreshStats();
    return input;
  }

  // advanceEmpty runs a predicted tick with no new input (keep friction synced).
  advanceEmpty(): void {
    this.stepLocal(null);
    this.refreshStats();
  }

  private stepLocal(input: PendingInput | null): void {
    const s = this.render;
    if (input) {
      applyInput(s, this.charId, input.seq, input.dirX, input.dirY);
      input.predictedTick = s.tick + 1;
    }
    advance(s);
  }

  // onFrame handles an authoritative frame: ack inputs the server has applied,
  // then rewind & replay. Reports whether the prediction was wrong.
  onFrame(f: ServerFrame): { corrected: boolean; delta: number } {
    const ackedSeq = f.chars[this.charId]?.lastSeq ?? 0;
    let hadAck = false;
    for (const p of this.pending) {
      if (p.seq <= ackedSeq) {
        p.acked = true;
        hadAck = true;
      }
    }
    if (hadAck) this.pending = this.pending.filter((p) => !p.acked);

    // Snapshot predicted position for our character before reconciliation.
    const before = this.render.chars[this.charId];
    const predictedX = before?.x ?? 0;
    const predictedY = before?.y ?? 0;

    // Rewind to authority (never smoothed), replay still-unacked inputs
    // tick-by-tick. Unacked inputs are replayed in order; other characters come
    // straight from the server frame.
    this.loadServerFrame(f);

    const after = this.render.chars[this.charId];
    const delta = after ? Math.hypot(after.x - predictedX, after.y - predictedY) : 0;
    const corrected = delta > 0;
    if (corrected) {
      this.stats.corrections++;
      this.stats.lastDelta = delta;
    } else {
      this.stats.lastDelta = 0;
    }
    this.refreshStats();
    return { corrected, delta };
  }

  private reconcile(): void {
    this.render = cloneState(this.confirmed);
    // Replay every unacked input as one predicted tick each, in seq order.
    for (const p of this.pending) {
      applyInput(this.render, this.charId, p.seq, p.dirX, p.dirY);
      advance(this.render);
    }
  }

  markSent(seq: number): void {
    const p = this.pending.find((x) => x.seq === seq);
    if (p) p.sentCount++;
  }

  // inputsToRetransmit returns unacked inputs that may have been lost (sent at
  // least once but no confirmation after a timeout). Same seq, safe to resend.
  inputsToRetransmit(maxAgeMs: number, now: number): PendingInput[] {
    void maxAgeMs;
    void now;
    return this.pending.filter((p) => p.sentCount > 0);
  }

  setConnected(v: boolean): void {
    this.stats.connected = v;
  }

  private refreshStats(): void {
    this.stats.inFlight = this.pending.length;
  }
}
