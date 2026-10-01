import type { CharState, Command, PendingInput, RefusedInput, SavedClientState, Snapshot } from './types';
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
	// reviewed holds finished inputs kept around for prediction-vs-authority
	// review: confirmed moves and final server refusals (with their reasons).
	reviewed: PendingInput[];
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
		reviewed: [],
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
	refused: RefusedInput[],
	cfg: PredConfig,
): ClientViewState {
	const chars = snap.chars ?? [];
	const me = chars.find((c) => c.id === charId);
	if (!me) return state;
	// Split finished inputs (server ack reached them, whether executed,
	// executed-but-blocked, or finally refused) from the still-in-flight tail.
	// The tail keeps its local seq order and is re-predicted from the new
	// authority base, so a partial offline refusal never reorders survivors.
	const refusalBySeq = new Map<number, RefusedInput>();
	for (const r of refused) {
		if (r.charId === charId) refusalBySeq.set(r.seq, r);
	}
	const finished: PendingInput[] = [];
	const stillPending: PendingInput[] = [];
	for (const p of state.pending) {
		if (p.seq > me.appliedSeq) {
			stillPending.push(p);
			continue;
		}
		const ref = refusalBySeq.get(p.seq);
		const done: PendingInput = { ...p, status: ref ? 'rejected' : 'confirmed' };
		if (ref) {
			done.msg = refusalText(ref.reason);
			done.reason = ref.reason;
		}
		finished.push(done);
	}
	let px = me.x;
	let py = me.y;
	let facing = me.facing;
	for (const p of stillPending) {
		const others = chars.filter((c) => c.id !== charId).map((c) => ({ x: c.x, y: c.y }));
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
		auth: chars,
		tick: snap.tick,
		hash: snap.hash,
		pending: stillPending,
		predX: px,
		predY: py,
		predFacing: facing,
		lastCorrection: correction,
		reviewed: [...state.reviewed, ...finished].slice(-30),
	};
}

// restoreView rebuilds a view from a persisted save so the pending queue, the
// last confirmed position and the predicted layer can keep reconciling after
// a mid-session save/reload.
export function restoreView(saved: SavedClientState): ClientViewState {
	const base = initialView(saved.view.auth);
	const view: ClientViewState = {
		...base,
		auth: saved.view.auth,
		tick: saved.view.tick,
		hash: saved.view.hash,
		pending: saved.pending.map((p) => ({ ...p })),
		predX: saved.view.predX,
		predY: saved.view.predY,
		predFacing: saved.view.predFacing,
		reviewed: [],
	};
	return view;
}

export function refusalText(reason: string): string {
	if (reason === 'offline-overflow') return '离线输入超出每 tick 准入上限，权威拒绝尾部输入';
	if (reason === 'offline-window') return '输入超出未确认窗口，权威拒绝该输入';
	return `服务端拒绝：${reason}`;
}
