import type { AppliedInput, CharState, Command, PendingInput, RejectedInput, Rules, Snapshot } from './types';
import { step, type PredConfig } from './predict';

export interface ClientViewState {
	ruleVersion: number;
	rules: Rules;
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
	predEnergy: number;
	rejectedInputs: RejectedInput[];
	lastCorrection: { fromX: number; fromY: number; toX: number; toY: number; at: number } | null;
}

export function initialView(initialAuth: ClientViewState['auth']): ClientViewState {
	return {
		ruleVersion: 1,
		rules: { version: 1, name: 'standard', costs: { up: 1, down: 1, left: 1, right: 1, wait: 0 } },
		auth: initialAuth,
		tick: 0,
		hash: '',
		pending: [],
		predX: initialAuth[0]?.x ?? 0,
		predY: initialAuth[0]?.y ?? 0,
		predFacing: initialAuth[0]?.facing ?? 'right',
		predEnergy: initialAuth[0]?.energy ?? 10,
		rejectedInputs: [],
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
	const ruleVersion = state.ruleVersion || 1;
	const ruleConfig = ruleVersion === 1 ? { ...cfg, blocked: [] } : cfg;
	const next = step(baseX, baseY, cmd, others, ruleConfig);
	const facing = cmd === 'wait' ? state.predFacing : cmd;
	const item: PendingInput = {
		seq,
		cmd,
		sentAt: Date.now(),
		status: 'predicting',
		ruleVersion,
		originalPredX: next.x,
		originalPredY: next.y,
		predX: next.x,
		predY: next.y,
	};
	return {
		...state,
		pending: [...state.pending, item],
		predX: next.x,
		predY: next.y,
		predFacing: facing,
		predEnergy: state.predEnergy - (cfg.costs?.[cmd] ?? 0),
	};
}

// applyAuthority folds a server snapshot into the view: confirmed pending
// inputs are removed; remaining ones are re-predicted from the new authority
// base, making any server-side deviation visible as a correction event.
export function applyAuthority(
	state: ClientViewState,
	charId: string,
	snap: Snapshot,
	applied: AppliedInput[],
	cfg: PredConfig,
): ClientViewState {
	const chars = snap.chars ?? [];
	const me = chars.find((c) => c.id === charId);
	if (!me || snap.tick < state.tick) return state;
	// Any pending input at or below the authority's ack is finished (confirmed
	// or executed-but-blocked). Only still-in-flight seqs are re-predicted.
	const mine = applied.filter((a) => a.charId === charId);
	const settled = new Map(mine.map((a) => [a.seq, a]));
	const needsConfirmation = new Set(mine.filter((a) => a.decision === 'needs_confirmation').map((a) => a.seq));
	const stillPending = state.pending.filter((p) => p.seq > me.appliedSeq || needsConfirmation.has(p.seq));
	const rejectedInputs = [...state.rejectedInputs];
	let correction = state.lastCorrection;
	const pendingBySeq = new Map(state.pending.map((p) => [p.seq, p]));
	const previousMe = state.auth.find((c) => c.id === charId);
	let authorityTailX = previousMe?.x ?? me.x;
	let authorityTailY = previousMe?.y ?? me.y;
	for (const rec of mine) {
		const p = pendingBySeq.get(rec.seq);
		if (!p) continue;
		if (rec.decision === 'needs_confirmation') {
			p.status = 'predicting';
			p.statusDetail = 'needs_confirmation';
			p.reason = rec.reason;
			p.ruleVersion = rec.ruleVersion ?? p.ruleVersion;
			p.predX = p.originalPredX ?? p.predX;
			p.predY = p.originalPredY ?? p.predY;
			continue;
		}
		const others = chars.filter((c) => c.id !== charId).map((c) => ({ x: c.x, y: c.y }));
		const projected = step(authorityTailX, authorityTailY, p.cmd, others, cfg);
		if (!rec.accepted) {
			if (p.predX !== authorityTailX || p.predY !== authorityTailY) {
				correction = { fromX: p.predX, fromY: p.predY, toX: authorityTailX, toY: authorityTailY, at: Date.now() };
			}
			rejectedInputs.push({
				...p,
				status: 'rejected',
				finalX: authorityTailX,
				finalY: authorityTailY,
				reason: rec.reason ?? 'blocked',
				msg: `原规则 v${rec.ruleVersion ?? p.ruleVersion ?? 1} 预测 (${p.originalPredX ?? p.predX},${p.originalPredY ?? p.predY})，当前规则 v${rec.effectiveVersion ?? snap.ruleVersion} 裁定 ${rec.decision ?? 'rejected'}：${rec.reason ?? 'blocked'}；最终 (${authorityTailX},${authorityTailY})`,
			});
		} else {
			authorityTailX = projected.x;
			authorityTailY = projected.y;
		}
	}
	for (const p of state.pending) {
		if (p.seq > me.appliedSeq) continue;
		const rec = settled.get(p.seq);
		if (!rec && (p.predX !== authorityTailX || p.predY !== authorityTailY)) {
			correction = { fromX: p.predX, fromY: p.predY, toX: authorityTailX, toY: authorityTailY, at: Date.now() };
		}
	}
	let px = me.x;
	let py = me.y;
	let facing = me.facing;
	for (const p of stillPending) {
		if (needsConfirmation.has(p.seq)) continue;
		const others = chars.filter((c) => c.id !== charId).map((c) => ({ x: c.x, y: c.y }));
		const n = step(px, py, p.cmd, others, cfg);
		px = n.x;
		py = n.y;
		if (p.cmd !== 'wait') facing = p.cmd;
		p.predX = px;
		p.predY = py;
		p.statusDetail = undefined;
	}
	// Correction is detected from the client's previous rendered layer:
	// before folding the snapshot, pending inputs predicted state.predX/Y;
	// after folding, the truth is either the server pos (nothing in flight) or
	// the re-predicted tail. Any mismatch snaps instead of smoothing.
	return {
		ruleVersion: snap.ruleVersion || state.ruleVersion || 1,
		rules: snap.rules ?? state.rules,
		auth: chars,
		tick: snap.tick,
		hash: snap.hash,
		pending: stillPending,
		predX: px,
		predY: py,
		predFacing: facing,
		predEnergy: me.energy ?? state.predEnergy,
		rejectedInputs,
		lastCorrection: correction,
	};
}

export function persistenceKey(roomId: string, charId: string): string {
	return `gameplatform:client:${roomId}:${charId}`;
}

export function saveView(roomId: string, charId: string, state: ClientViewState, nextSeq: number): void {
	const stored = {
		nextSeq,
		view: {
			ruleVersion: state.ruleVersion,
			rules: state.rules,
			auth: state.auth,
			tick: state.tick,
			hash: state.hash,
			pending: state.pending,
			predX: state.predX,
			predY: state.predY,
			predFacing: state.predFacing,
			predEnergy: state.predEnergy,
			rejectedInputs: state.rejectedInputs.slice(-50),
		},
	};
	localStorage.setItem(persistenceKey(roomId, charId), JSON.stringify(stored));
}

export function loadView(roomId: string, charId: string): { view: ClientViewState; nextSeq: number } | null {
	const raw = localStorage.getItem(persistenceKey(roomId, charId));
	if (!raw) return null;
	try {
		const parsed = JSON.parse(raw) as { nextSeq: number; view: ClientViewState };
		if (!Array.isArray(parsed.view?.auth) || !Array.isArray(parsed.view?.pending)) return null;
		parsed.view.ruleVersion ??= 1;
		parsed.view.rules ??= { version: 1, name: 'standard' };
		parsed.view.predEnergy ??= parsed.view.auth[0]?.energy ?? 10;
		parsed.view.rejectedInputs ??= [];
		parsed.view.lastCorrection = null;
		return parsed;
	} catch {
		return null;
	}
}
