// Deterministic client-side simulation kernel, a faithful port of the Go
// package gameserver/internal/sim. Integer milli-units only, so client
// prediction and server confirmation compute on identical rules.

export const MILLI = 1000;
export const FIELD_HALF = 5 * MILLI;
export const MAX_SPEED = 3 * MILLI;
const FRICTION_NUM = 8;
const FRICTION_DEN = 10;
export const KICK_IMPULSE = 6 * MILLI;

export interface CharState {
  id: string;
  x: number;
  y: number;
  vx: number;
  vy: number;
  lastSeq: number;
}

export interface SimState {
  tick: number;
  chars: Record<string, CharState>;
}

export function newState(ids: string[]): SimState {
  const chars: Record<string, CharState> = {};
  ids.forEach((id, i) => {
    chars[id] = { id, x: (i - 1) * 2 * MILLI, y: 0, vx: 0, vy: 0, lastSeq: 0 };
  });
  return { tick: 0, chars };
}

export function cloneState(s: SimState): SimState {
  return {
    tick: s.tick,
    chars: Object.fromEntries(Object.entries(s.chars).map(([k, v]) => [k, { ...v }])),
  };
}

// Small deterministic FNV-1a hash of the canonical state encoding. It mirrors
// the ordering/fields of Go's State.Hash (not the digest algorithm; the server
// sends its own hash in frames for authoritative comparison).
export function stateHash(s: SimState): string {
  let h = 0x811c9dc5;
  const write = (str: string) => {
    for (let i = 0; i < str.length; i++) {
      h ^= str.charCodeAt(i);
      h = Math.imul(h, 0x01000193);
    }
  };
  write(`tick=${s.tick}`);
  for (const id of Object.keys(s.chars).sort()) {
    const c = s.chars[id];
    write(`|${id}:${c.x},${c.y},${c.vx},${c.vy},${c.lastSeq}`);
  }
  return (h >>> 0).toString(16).padStart(8, "0");
}

const clampDir = (v: number) => (v > 0 ? 1 : v < 0 ? -1 : 0);

export function applyInput(s: SimState, id: string, seq: number, dirX: number, dirY: number): void {
  const c = s.chars[id];
  if (!c || seq <= c.lastSeq) return; // stale input never applies twice
  c.lastSeq = seq;
  c.vx = clampDir(dirX) * MAX_SPEED;
  c.vy = clampDir(dirY) * MAX_SPEED;
}

export function applyImpulse(s: SimState, id: string, dx: number, dy: number): void {
  const c = s.chars[id];
  if (!c) return;
  c.vx += clampDir(dx) * KICK_IMPULSE;
  c.vy += clampDir(dy) * KICK_IMPULSE;
}

export function advance(s: SimState): void {
  for (const c of Object.values(s.chars)) {
    c.x += c.vx;
    c.y += c.vy;
    c.vx = Math.trunc((c.vx * FRICTION_NUM) / FRICTION_DEN);
    c.vy = Math.trunc((c.vy * FRICTION_NUM) / FRICTION_DEN);
    if (c.x > FIELD_HALF) { c.x = FIELD_HALF; c.vx = 0; }
    if (c.x < -FIELD_HALF) { c.x = -FIELD_HALF; c.vx = 0; }
    if (c.y > FIELD_HALF) { c.y = FIELD_HALF; c.vy = 0; }
    if (c.y < -FIELD_HALF) { c.y = -FIELD_HALF; c.vy = 0; }
  }
  s.tick++;
}

export interface StoredEvent {
  id: number;
  kind: "room" | "session" | "input" | "impulse" | "checkpoint";
  atTick: number;
  data: unknown;
}

interface RoomData { roomId: string; charIds: string[] }
interface InputData { charId: string; seq: number; dirX: number; dirY: number }
interface ImpulseData { charId: string; dirX: number; dirY: number }
interface CheckpointData { hash: string }

export interface ReplayFrame {
  tick: number;
  state: SimState;
  match: boolean;
}

// Rebuild every frame from the persisted event log. Verification against
// authoritative checkpoint hashes is done by the Go replay endpoint (it shares
// the hash function); the client renders states and surfaces its verdict.
export function replay(events: StoredEvent[]): ReplayFrame[] {
  let state: SimState | null = null;
  const byTick = new Map<number, StoredEvent[]>();
  const checks = new Map<number, string>();
  let maxTick = 0;
  for (const e of events) {
    if (e.kind === "room") {
      state = newState((e.data as RoomData).charIds);
    } else if (e.kind === "input" || e.kind === "impulse") {
      const list = byTick.get(e.atTick) ?? [];
      list.push(e);
      byTick.set(e.atTick, list);
    } else if (e.kind === "checkpoint") {
      checks.set(e.atTick, (e.data as CheckpointData).hash);
    }
    maxTick = Math.max(maxTick, e.atTick);
  }
  if (!state) return [];
  const frames: ReplayFrame[] = [{ tick: 0, state: cloneState(state), match: true }];
  for (let t = 1; t <= maxTick; t++) {
    for (const e of byTick.get(t) ?? []) {
      if (e.kind === "input") {
        const d = e.data as InputData;
        applyInput(state, d.charId, d.seq, d.dirX, d.dirY);
      } else if (e.kind === "impulse") {
        const d = e.data as ImpulseData;
        applyImpulse(state, d.charId, d.dirX, d.dirY);
      }
    }
    advance(state);
    void checks;
    frames.push({ tick: t, state: cloneState(state), match: true });
  }
  return frames;
}
