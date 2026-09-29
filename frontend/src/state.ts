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

export function initialView(initialAuth: ClientViewState['auth']): ClientViewState {
	return {
		auth: initialAuth,
		tick: 0,
		hash: '',
		pending: [],
		predX: initialAuth[0]?.x ?? 0,
		predY: initialAuth[0]?.y ?? 0,
		predFacing: initialAuth[0]?.facing ?? 'right',
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
	// Any pending input at or below the authority's ack is finished (confirmed
	// or executed-but-blocked). Only still-in-flight seqs are re-predicted.
	const stillPending = state.pending.filter((p) => p.seq > me.appliedSeq);
	const newlyConfirmed = state.pending.filter((p) => appliedSeqs.includes(p.seq));
	newlyConfirmed.forEach((p) => {
		p.status = 'confirmed';
	});
	let px = me.x;
	let py = me.y;
	let facing = me.facing;
	for (const p of stillPending) {
		const others = snap.chars.filter((c) => c.id !== charId).map((c) => ({ x: c.x, y: c.y }));
		const n = step(px, py, p.cmd, others, cfg);
		px = n.x;
		py = n.y;
		if (p.cmd !== 'wait') facing = p.cmd;
		p.predX = px;
		p.predY = py;
	}
	// Correction is detected from the client's previous rendered layer:
	// before folding the snapshot, pending inputs predicted state.predX/Y;
	// after folding, the truth is either the server pos (nothing in flight) or
	// the re-predicted tail. Any mismatch snaps instead of smoothing.
	const corrected = state.predX !== px || state.predY !== py;
	const correction =
		corrected
			? { fromX: state.predX, fromY: state.predY, toX: me.x, toY: me.y, at: Date.now() }
			: state.lastCorrection;
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
