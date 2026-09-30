import type { CharState, Command, PendingInput, Snapshot } from './types';
import { step, type PredConfig } from './predict';

export interface ClientViewState {
	// authoritative state of every character
	auth: CharState[];
	tick: number;
	hash: string;
	// local pending inputs (predicted, not yet confirmed)
	pending: PendingInput[];
	// predicted position of the locally owned character (last unacked layer)
	predX: number;
	predY: number;
	predFacing: Command;
	lastCorrection: { fromX: number; fromY: number; toX: number; toY: number; at: number } | null;
}

export function initialView(initialAuth: ClientViewState['auth'], charId?: string): ClientViewState {
	const own = initialAuth.find((c) => c.id === charId) ?? initialAuth[0];
	return {
		auth: initialAuth,
		tick: 0,
		hash: '',
		pending: [],
		predX: own?.x ?? 0,
		predY: own?.y ?? 0,
		predFacing: own?.facing ?? 'right',
		lastCorrection: null,
	};
}

// applyLocalInput layers one new predicted move on top of authority + pending.
export function applyLocalInput(
	state: ClientViewState,
	charId: string,
	seq: number,
	cmd: Command,
	cfg: PredConfig,
): ClientViewState {
	const me = state.auth.find((c) => c.id === charId);
	if (!me) return state;
	const baseX = state.pending.length ? state.predX : me.x;
	const baseY = state.pending.length ? state.predY : me.y;
	const others = state.auth.filter((c) => c.id !== charId).map((c) => ({ x: c.x, y: c.y }));
	const next = step(baseX, baseY, cmd, others, cfg);
	const facing = cmd === 'wait' ? state.predFacing : cmd;
	const item: PendingInput = {
		seq,
		cmd,
		sentAt: Date.now(),
		status: 'predicting',
		predX: next.x,
		predY: next.y,
	};
	return {
		...state,
		pending: [...state.pending, item],
		predX: next.x,
		predY: next.y,
		predFacing: facing,
	};
}

// applyAuthority folds a server snapshot into the view: confirmed pending
// inputs are removed; remaining ones are re-predicted from the new authority
// base, making any server-side deviation visible as a correction event.
export function applyAuthority(
	state: ClientViewState,
	charId: string,
	snap: Snapshot,
	appliedSeqs: number[],
	cfg: PredConfig,
): ClientViewState {
	const me = snap.chars.find((c) => c.id === charId);
	if (!me) return state;
	// Any pending input at or below the authority's ack is finished. A seq
	// can be confirmed in two ways:
	//   - it is listed in this event's applied[] (executed or blocked), or
	//   - the snapshot's appliedSeq jumped past it (backlog replay after a
	//     reconnect fills events whose applied[] we never rendered).
	const acked = new Set(appliedSeqs);
	const confirmed = state.pending.filter((p) => p.seq <= me.appliedSeq || acked.has(p.seq));
	const stillPending = state.pending.filter((p) => p.seq > me.appliedSeq && !acked.has(p.seq));
	confirmed.forEach((p) => {
		p.status = 'confirmed';
	});
	// Correction detection for confirmed inputs: replay the just-confirmed
	// prefix on top of the PREVIOUS authoritative base and compare the last
	// predicted cell with the server's actual position. Comparing the final
	// rendered layer (as before) reported phantom corrections whenever
	// unacked inputs remained in flight, because their re-prediction tail
	// legitimately differs from the confirmed base.
	let correction = state.lastCorrection;
	if (confirmed.length > 0) {
		const prevMe = state.auth.find((c) => c.id === charId);
		let rx = prevMe?.x ?? me.x;
		let ry = prevMe?.y ?? me.y;
		for (const p of confirmed) {
			const others = state.auth.filter((c) => c.id !== charId).map((c) => ({ x: c.x, y: c.y }));
			const n = step(rx, ry, p.cmd, others, cfg);
			rx = n.x;
			ry = n.y;
		}
		if (rx !== me.x || ry !== me.y) {
			correction = { fromX: rx, fromY: ry, toX: me.x, toY: me.y, at: Date.now() };
		}
	}
	// Re-predict the still-in-flight tail from the new authority base.
	// Other characters move too (simultaneous input), so their LATEST
	// snapshot positions are used for every step; our own tail is layered.
	let px = me.x;
	let py = me.y;
	let facing = me.facing;
	for (const p of stillPending) {
		let n = step(px, py, p.cmd, snap.chars.filter((c) => c.id !== charId).map((c) => ({ x: c.x, y: c.y })), cfg);
		px = n.x;
		py = n.y;
		if (p.cmd !== 'wait') facing = p.cmd;
		p.predX = px;
		p.predY = py;
	}
	return {
		auth: snap.chars,
		tick: snap.tick,
		hash: snap.hash,
		pending: stillPending,
		predX: px,
		predY: py,
		predFacing: facing,
		lastCorrection: correction,
	};
}
