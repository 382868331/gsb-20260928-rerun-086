import { describe, expect, it, vi } from 'vitest';
import { applyAuthority, applyLocalInput, initialView, loadView, saveView } from './state';
import type { AppliedInput, Command, Snapshot } from './types';

function applied(seq: number, accepted: boolean, x: number, y: number, charId = 'c1'): AppliedInput {
	return { tick: 1, sessionId: 's1', charId, seq, cmd: 'right', accepted, x, y };
}

const cfg = { width: 6, height: 4, obstacles: [[2, 0]] as [number, number][] };

function start() {
	let s = initialView([
		{ id: 'c1', x: 1, y: 1, facing: 'right', appliedSeq: 0 },
		{ id: 'c2', x: 4, y: 1, facing: 'left', appliedSeq: 0 },
	]);
	// bind local char by re-deriving view with c1 pending state via helper
	s = { ...s, predX: 1, predY: 1, predFacing: 'right' };
	return s;
}

describe('client prediction and authority reconciliation', () => {
	it('survives an idle heartbeat snapshot with no applied inputs', () => {
		const s0 = start();
		const snap = { tick: 7, hash: 'hb', chars: undefined } as unknown as Snapshot;
		const s1 = applyAuthority(s0, 'c1', snap, [], cfg);
		expect(s1).toBe(s0);
		const snap2 = { tick: 8, hash: 'hb2', chars: null } as unknown as Snapshot;
		expect(applyAuthority(s0, 'c1', snap2, [], cfg)).toBe(s0);
	});

	it('predicts locally before the server acks', () => {
		const s = applyLocalInput(start(), 'c1', 1, 'right', cfg);
		expect(s.pending).toHaveLength(1);
		expect([s.predX, s.predY]).toEqual([2, 1]);
	});

	it('clears pending on confirmation and snaps to authority with no smoothing', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'right', cfg);
		const snap: Snapshot = {
			tick: 1,
			hash: 'h1',
			chars: [
				{ id: 'c1', x: 2, y: 1, facing: 'right', appliedSeq: 1 },
				{ id: 'c2', x: 4, y: 1, facing: 'left', appliedSeq: 0 },
			],
		};
		s = applyAuthority(s, 'c1', snap, [applied(1, true, 2, 1)], cfg);
		expect(s.pending).toHaveLength(0);
		expect([s.predX, s.predY]).toEqual([2, 1]);
	});

	it('records a correction when the authority result differs from prediction', () => {
		// Client predicts moving right into (2,1); server says the move was
		// blocked (e.g. another char took the cell) and stays at (1,1).
		let s = applyLocalInput(start(), 'c1', 1, 'right', cfg);
		const snap: Snapshot = {
			tick: 1,
			hash: 'h-block',
			chars: [
				{ id: 'c1', x: 1, y: 1, facing: 'right', appliedSeq: 1 },
				{ id: 'c2', x: 2, y: 1, facing: 'left', appliedSeq: 1 },
			],
		};
		s = applyAuthority(s, 'c1', snap, [applied(1, false, 1, 1)], cfg);
		expect([s.predX, s.predY]).toEqual([1, 1]);
		expect(s.lastCorrection).not.toBeNull();
		expect(s.lastCorrection!.fromX).toBe(2);
		expect(s.lastCorrection!.toX).toBe(1);
	});

	it('keeps unacked inputs pending across snapshots and re-predicts the tail', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'right', cfg);
		s = applyLocalInput(s, 'c1', 2, 'right', cfg);
		const snap: Snapshot = {
			tick: 1,
		hash: 'h',
			chars: [
				{ id: 'c1', x: 2, y: 1, facing: 'right', appliedSeq: 1 },
				{ id: 'c2', x: 4, y: 1, facing: 'left', appliedSeq: 0 },
			],
		};
		s = applyAuthority(s, 'c1', snap, [applied(1, true, 2, 1)], cfg);
		expect(s.pending.map((p) => p.seq)).toEqual([2]);
		expect([s.predX, s.predY]).toEqual([3, 1]);
	});

	it('keeps later unacked inputs after recording an authority rejection', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'right', cfg);
		s = applyLocalInput(s, 'c1', 2, 'right', cfg);
		const snap: Snapshot = {
			tick: 1,
			hash: 'reject',
			chars: [
				{ id: 'c1', x: 1, y: 1, facing: 'right', appliedSeq: 1 },
				{ id: 'c2', x: 2, y: 1, facing: 'left', appliedSeq: 0 },
			],
		};
		s = applyAuthority(s, 'c1', snap, [applied(1, false, 1, 1)], cfg);
		expect(s.pending.map((p) => p.seq)).toEqual([2]);
		expect(s.rejectedInputs).toHaveLength(1);
		expect(s.rejectedInputs[0].finalX).toBe(1);
		expect([s.predX, s.predY]).toEqual([1, 1]);
	});

	it('re-predicts accepted inputs following an authority rejection in seq order', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'right', cfg);
		s = applyLocalInput(s, 'c1', 2, 'up', cfg);
		const snap: Snapshot = {
			tick: 1,
			hash: 'partial-reject',
			chars: [
				{ id: 'c1', x: 1, y: 0, facing: 'up', appliedSeq: 2 },
				{ id: 'c2', x: 4, y: 1, facing: 'left', appliedSeq: 0 },
			],
		};
		s = applyAuthority(
			s,
			'c1',
			snap,
			[applied(1, false, 1, 1), { ...applied(2, true, 1, 0), cmd: 'up' }],
			cfg,
		);
		expect(s.pending).toHaveLength(0);
		expect(s.rejectedInputs.map((p) => p.seq)).toEqual([1]);
		expect([s.predX, s.predY]).toEqual([1, 0]);
	});

	it('saves and restores pending inputs, ack watermark and predicted layer', () => {
		const store = new Map<string, string>();
		vi.stubGlobal('localStorage', {
			getItem: (key: string) => store.get(key) ?? null,
			setItem: (key: string, value: string) => void store.set(key, value),
		});
		const s = applyLocalInput(start(), 'c1', 1, 'right', cfg);
		saveView('room-save', 'c1', s, 1);
		const restored = loadView('room-save', 'c1');
		expect(restored?.nextSeq).toBe(1);
		expect(restored?.view.pending.map((p) => p.seq)).toEqual([1]);
		expect(restored?.view.auth.find((c) => c.id === 'c1')?.appliedSeq).toBe(0);
		expect([restored?.view.predX ?? 0, restored?.view.predY ?? 0]).toEqual([2, 1]);
	});
});

describe('rule versioning', () => {
	it('predicts around sealed cells of the current rules', () => {
		const s0 = { ...start(), rules: { version: 2, sealed: [[2, 1]] as [number, number][], costs: {} } };
		const s = applyLocalInput(s0, 'c1', 1, 'right', cfg);
		// (2,1) is sealed under v2: prediction stays put, basis recorded as v2.
		expect([s.predX, s.predY]).toEqual([1, 1]);
		expect(s.pending[0].ruleVersion).toBe(2);
	});

	it('keeps the original rule basis on pending inputs across a rule switch', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'right', cfg); // formed under v1
		const snap: Snapshot = {
			tick: 1,
			hash: 'h-rules',
			chars: [
				{ id: 'c1', x: 1, y: 1, facing: 'right', appliedSeq: 0 },
				{ id: 'c2', x: 4, y: 1, facing: 'left', appliedSeq: 0 },
			],
		};
		// Heartbeat carries the new rules; the pending input keeps basis v1.
		s = applyAuthority(s, 'c1', snap, [], cfg, { version: 2, sealed: [[2, 1]], costs: {} });
		expect(s.rules.version).toBe(2);
		expect(s.pending[0].ruleVersion).toBe(1);
	});

	it('records rejection with reason, applied version and reconfirm flag', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'right', cfg); // basis v1, predicts (2,1)
		const snap: Snapshot = {
			tick: 1,
			hash: 'h-sealed',
			chars: [
				{ id: 'c1', x: 1, y: 1, facing: 'right', appliedSeq: 1 },
				{ id: 'c2', x: 4, y: 1, facing: 'left', appliedSeq: 0 },
			],
		};
		s = applyAuthority(
			s,
			'c1',
			snap,
			[{ ...applied(1, false, 1, 1), ruleVersion: 1, appliedRuleVersion: 2, reason: 'sealed', needsReconfirm: true }],
			cfg,
			{ version: 2, sealed: [[2, 1]], costs: {} },
		);
		expect(s.rejectedInputs).toHaveLength(1);
		const rej = s.rejectedInputs[0];
		expect(rej.ruleVersion).toBe(1);
		expect(rej.appliedRuleVersion).toBe(2);
		expect(rej.reason).toBe('sealed');
		expect(rej.needsReconfirm).toBe(true);
		expect([s.predX, s.predY]).toEqual([1, 1]);
	});

	it('save/restore keeps pending rule basis and rejected records', () => {
		const store = new Map<string, string>();
		vi.stubGlobal('localStorage', {
			getItem: (key: string) => store.get(key) ?? null,
			setItem: (key: string, value: string) => void store.set(key, value),
		});
		let s = applyLocalInput(start(), 'c1', 1, 'right', cfg);
		const snap: Snapshot = {
			tick: 1,
			hash: 'h',
			chars: [
				{ id: 'c1', x: 1, y: 1, facing: 'right', appliedSeq: 1 },
				{ id: 'c2', x: 4, y: 1, facing: 'left', appliedSeq: 0 },
			],
		};
		s = applyAuthority(
			s,
			'c1',
			snap,
			[{ ...applied(1, false, 1, 1), ruleVersion: 1, appliedRuleVersion: 2, reason: 'sealed', needsReconfirm: true }],
			cfg,
			{ version: 2, sealed: [[2, 1]], costs: {} },
		);
		s = applyLocalInput(s, 'c1', 2, 'up', cfg); // new input under v2
		saveView('room-rules', 'c1', s, 2);
		const restored = loadView('room-rules', 'c1');
		expect(restored?.view.rules.version).toBe(2);
		expect(restored?.view.pending[0].ruleVersion).toBe(2);
		expect(restored?.view.rejectedInputs[0].needsReconfirm).toBe(true);
	});
});

describe('shared resource adjudication', () => {
	function claimRec(seq: number, charId: string, accepted: boolean, tick: number, reason?: AppliedInput['reason']): AppliedInput {
		return { tick, sessionId: `s-${charId}`, charId, seq, cmd: 'claim' as Command, accepted, x: 1, y: 1, reason };
	}

	it('predicts a claim locally without moving the character', () => {
		const s = applyLocalInput(start(), 'c1', 1, 'claim', cfg);
		expect(s.pending).toHaveLength(1);
		expect(s.pending[0].cmd).toBe('claim');
		expect([s.predX, s.predY]).toEqual([1, 1]);
	});

	it('folds the authority verdict when the OTHER client won the slot', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'claim', cfg); // formed offline
		// Tick 1 (while c1 was offline): c2's claim was granted the only slot.
		s = applyAuthority(s, 'c1', {
			tick: 1, hash: 'h1', passesLeft: 0,
			chars: [
				{ id: 'c1', x: 1, y: 1, facing: 'right', appliedSeq: 0 },
				{ id: 'c2', x: 4, y: 1, facing: 'left', appliedSeq: 1, passes: 1 },
			],
		}, [claimRec(1, 'c2', true, 1)], cfg);
		expect(s.passesLeft).toBe(0);
		expect(s.claims).toHaveLength(1);
		expect(s.claims[0]).toMatchObject({ charId: 'c2', granted: true });
		expect(s.pending).toHaveLength(1); // own claim still unconfirmed

		// Tick 2: c1's claim is rejected — the pool was already empty.
		s = applyAuthority(s, 'c1', {
			tick: 2, hash: 'h2', passesLeft: 0,
			chars: [
				{ id: 'c1', x: 1, y: 1, facing: 'right', appliedSeq: 1 },
				{ id: 'c2', x: 4, y: 1, facing: 'left', appliedSeq: 1, passes: 1 },
			],
		}, [claimRec(1, 'c1', false, 2, 'resource-exhausted')], cfg);
		expect(s.pending).toHaveLength(0);
		expect(s.claims).toHaveLength(2);
		expect(s.claims[1]).toMatchObject({ charId: 'c1', granted: false, reason: 'resource-exhausted' });
		// The rejection is recorded with its reason and never erased by the
		// corrected (identical-looking) final state.
		expect(s.rejectedInputs).toHaveLength(1);
		expect(s.rejectedInputs[0].reason).toBe('resource-exhausted');
		expect(s.rejectedInputs[0].msg).toContain('名额');
		expect([s.predX, s.predY]).toEqual([1, 1]);
	});

	it('records a granted claim without a rejection entry', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'claim', cfg);
		s = applyAuthority(s, 'c1', {
			tick: 1, hash: 'h1', passesLeft: 0,
			chars: [
				{ id: 'c1', x: 1, y: 1, facing: 'right', appliedSeq: 1, passes: 1 },
				{ id: 'c2', x: 4, y: 1, facing: 'left', appliedSeq: 0 },
			],
		}, [claimRec(1, 'c1', true, 1)], cfg);
		expect(s.claims).toHaveLength(1);
		expect(s.claims[0].granted).toBe(true);
		expect(s.rejectedInputs).toHaveLength(0);
		expect(s.passesLeft).toBe(0);
	});

	it('keeps dependency info and gives the dependent action its own reason', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'claim', cfg);
		s = applyLocalInput(s, 'c1', 2, 'right', cfg, 1); // depends on seq 1
		expect(s.pending[1].dependsOn).toBe(1);
		s = applyAuthority(s, 'c1', {
			tick: 1, hash: 'h-dep', passesLeft: 0,
			chars: [
				{ id: 'c1', x: 1, y: 1, facing: 'right', appliedSeq: 2 },
				{ id: 'c2', x: 4, y: 1, facing: 'left', appliedSeq: 0, passes: 1 },
			],
		}, [
			claimRec(1, 'c1', false, 1, 'resource-exhausted'),
			{ ...applied(2, false, 1, 1), reason: 'dependency-rejected' as const, dependsOn: 1 },
		], cfg);
		expect(s.rejectedInputs).toHaveLength(2);
		expect(s.rejectedInputs[0].reason).toBe('resource-exhausted');
		expect(s.rejectedInputs[1].reason).toBe('dependency-rejected');
		expect(s.rejectedInputs[1].dependsOn).toBe(1);
	});

	it('does not duplicate verdicts when a snapshot is folded twice', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'claim', cfg);
		const snap: Snapshot = {
			tick: 1, hash: 'h1', passesLeft: 0,
			chars: [
				{ id: 'c1', x: 1, y: 1, facing: 'right', appliedSeq: 1, passes: 1 },
				{ id: 'c2', x: 4, y: 1, facing: 'left', appliedSeq: 0 },
			],
		};
		s = applyAuthority(s, 'c1', snap, [claimRec(1, 'c1', true, 1)], cfg);
		s = applyAuthority(s, 'c1', snap, [claimRec(1, 'c1', true, 1)], cfg);
		expect(s.claims).toHaveLength(1);
	});

	it('save/restore keeps verdicts, pool state and pending dependencies', () => {
		const store = new Map<string, string>();
		vi.stubGlobal('localStorage', {
			getItem: (key: string) => store.get(key) ?? null,
			setItem: (key: string, value: string) => void store.set(key, value),
		});
		let s = applyLocalInput(start(), 'c1', 1, 'claim', cfg);
		s = applyAuthority(s, 'c1', {
			tick: 1, hash: 'h1', passesLeft: 0,
			chars: [
				{ id: 'c1', x: 1, y: 1, facing: 'right', appliedSeq: 1, passes: 1 },
				{ id: 'c2', x: 4, y: 1, facing: 'left', appliedSeq: 0 },
			],
		}, [claimRec(1, 'c1', true, 1)], cfg);
		s = applyLocalInput(s, 'c1', 2, 'right', cfg, 1);
		saveView('room-res', 'c1', s, 2);
		const restored = loadView('room-res', 'c1');
		expect(restored?.view.passesLeft).toBe(0);
		expect(restored?.view.claims).toHaveLength(1);
		expect(restored?.view.claims[0].granted).toBe(true);
		expect(restored?.view.pending[0].dependsOn).toBe(1);
	});
});
