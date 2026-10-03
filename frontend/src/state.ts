import type { AppliedInput, CharState, ClaimVerdict, Command, PendingInput, RejectedInput, Rules, Snapshot } from './types';
import { step, type PredConfig } from './predict';

export interface ClientViewState {
	// authoritative state of every character
	auth: CharState[];
	tick: number;
	hash: string;
	// rules version the authority currently applies (learned from events)
	rules: Rules;
	// remaining shared-resource pool, from the authoritative snapshot
	passesLeft: number;
	// recent shared-resource adjudications for BOTH clients (authority order)
	claims: ClaimVerdict[];
	// local pending inputs (predicted, not yet confirmed)
	pending: PendingInput[];
	// predicted position of the locally owned character (last unacked layer)
	predX: number;
	predY: number;
	predFacing: Command;
	rejectedInputs: RejectedInput[];
	lastCorrection: { fromX: number; fromY: number; toX: number; toY: number; at: number } | null;
}

export function initialView(initialAuth: ClientViewState['auth'], rules?: Rules, passesLeft = 0): ClientViewState {
	return {
		auth: initialAuth,
		tick: 0,
		hash: '',
		rules: rules ?? { version: 1, costs: {} },
		passesLeft,
		claims: [],
		pending: [],
		predX: initialAuth[0]?.x ?? 0,
		predY: initialAuth[0]?.y ?? 0,
		predFacing: initialAuth[0]?.facing ?? 'right',
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
	dependsOn = 0,
): ClientViewState {
	const me = state.auth.find((c) => c.id === charId);
	if (!me) return state;
	const effCfg = { ...cfg, sealed: state.rules.sealed };
	const baseX = state.pending.length ? state.predX : me.x;
	const baseY = state.pending.length ? state.predY : me.y;
	const others = state.auth.filter((c) => c.id !== charId).map((c) => ({ x: c.x, y: c.y }));
	const next = step(baseX, baseY, cmd, others, effCfg);
	const facing = cmd === 'wait' || cmd === 'claim' ? state.predFacing : cmd;
	const item: PendingInput = {
		seq,
		cmd,
		sentAt: Date.now(),
		status: 'predicting',
		predX: next.x,
		predY: next.y,
		ruleVersion: state.rules.version,
		dependsOn: dependsOn > 0 ? dependsOn : undefined,
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
	applied: AppliedInput[],
	cfg: PredConfig,
	rules?: Rules,
): ClientViewState {
	const chars = snap.chars ?? [];
	const me = chars.find((c) => c.id === charId);
	if (!me || snap.tick < state.tick) return state;
	const nextRules = rules ?? state.rules;
	const effCfg = { ...cfg, sealed: nextRules.sealed };
	// Any pending input at or below the authority's ack is finished (confirmed
	// or executed-but-blocked). Only still-in-flight seqs are re-predicted.
	const stillPending = state.pending.filter((p) => p.seq > me.appliedSeq);
	const mine = applied.filter((a) => a.charId === charId);
	const settled = new Map(mine.map((a) => [a.seq, a]));
	const rejectedInputs = [...state.rejectedInputs];
	// Shared-resource adjudications from this tick, for BOTH clients: the
	// same authoritative verdict list is folded into every client's view,
	// so no client's last-arriving prediction becomes the common result.
	const claims = [...state.claims];
	for (const a of applied) {
		if (a.cmd !== 'claim') continue;
		if (claims.some((v) => v.charId === a.charId && v.seq === a.seq)) continue;
		claims.push({
			tick: a.tick,
			charId: a.charId,
			seq: a.seq,
			granted: a.accepted,
			reason: a.accepted ? undefined : (a.reason ?? 'resource-exhausted'),
			ruleVersion: a.ruleVersion,
			appliedRuleVersion: a.appliedRuleVersion,
		});
	}
	while (claims.length > 20) claims.shift();
	let correction = state.lastCorrection;
	const pendingBySeq = new Map(state.pending.map((p) => [p.seq, p]));
	const previousMe = state.auth.find((c) => c.id === charId);
	let authorityTailX = previousMe?.x ?? me.x;
	let authorityTailY = previousMe?.y ?? me.y;
	for (const rec of mine) {
		const p = pendingBySeq.get(rec.seq);
		if (!p) continue;
		const others = chars.filter((c) => c.id !== charId).map((c) => ({ x: c.x, y: c.y }));
		const projected = step(authorityTailX, authorityTailY, p.cmd, others, effCfg);
		if (!rec.accepted) {
			if (p.predX !== authorityTailX || p.predY !== authorityTailY) {
				correction = { fromX: p.predX, fromY: p.predY, toX: authorityTailX, toY: authorityTailY, at: Date.now() };
			}
			const reason = rec.reason ?? 'blocked';
			const msg = p.cmd === 'claim'
				? `依据 v${p.ruleVersion} 预测取得名额，权威按 v${rec.appliedRuleVersion ?? p.ruleVersion} 判定${reasonLabel(reason)}`
				: `依据 v${p.ruleVersion} 预测 (${p.predX},${p.predY})，权威按 v${rec.appliedRuleVersion ?? p.ruleVersion} 判定${reasonLabel(reason)}，最终 (${authorityTailX},${authorityTailY})`;
			rejectedInputs.push({
				...p,
				status: 'rejected',
				finalX: authorityTailX,
				finalY: authorityTailY,
				reason,
				appliedRuleVersion: rec.appliedRuleVersion,
				needsReconfirm: rec.needsReconfirm,
				msg,
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
		const others = chars.filter((c) => c.id !== charId).map((c) => ({ x: c.x, y: c.y }));
		const n = step(px, py, p.cmd, others, effCfg);
		px = n.x;
		py = n.y;
		if (p.cmd !== 'wait' && p.cmd !== 'claim') facing = p.cmd;
		// Re-predicted for rendering only: p.ruleVersion keeps the original
		// rule basis so the correction never erases it.
		p.predX = px;
		p.predY = py;
	}
	// Correction is detected from the client's previous rendered layer:
	// before folding the snapshot, pending inputs predicted state.predX/Y;
	// after folding, the truth is either the server pos (nothing in flight) or
	// the re-predicted tail. Any mismatch snaps instead of smoothing.
	return {
		auth: chars,
		tick: snap.tick,
		hash: snap.hash,
		rules: nextRules,
		passesLeft: snap.passesLeft ?? state.passesLeft,
		claims,
		pending: stillPending,
		predX: px,
		predY: py,
		predFacing: facing,
		rejectedInputs,
		lastCorrection: correction,
	};
}

export function reasonLabel(reason: string): string {
	switch (reason) {
		case 'sealed':
			return '拒绝（区域已封闭）';
		case 'insufficient-energy':
			return '拒绝（能量不足）';
		case 'resource-exhausted':
			return '拒绝（名额已被其他客户端取得）';
		case 'dependency-rejected':
			return '拒绝（所依赖的动作未成功）';
		case 'claims-closed':
			return '拒绝（当前规则禁止领取名额）';
		default:
			return '阻挡';
	}
}

export function persistenceKey(roomId: string, charId: string): string {
	return `gameplatform:client:${roomId}:${charId}`;
}

export function saveView(roomId: string, charId: string, state: ClientViewState, nextSeq: number): void {
	const stored = {
		nextSeq,
		view: {
			auth: state.auth,
			tick: state.tick,
			hash: state.hash,
			rules: state.rules,
			passesLeft: state.passesLeft,
			claims: state.claims.slice(-20),
			pending: state.pending,
			predX: state.predX,
			predY: state.predY,
			predFacing: state.predFacing,
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
		parsed.view.rejectedInputs ??= [];
		parsed.view.rules ??= { version: 1, costs: {} };
		parsed.view.passesLeft ??= 0;
		parsed.view.claims ??= [];
		parsed.view.lastCorrection = null;
		return parsed;
	} catch {
		return null;
	}
}
