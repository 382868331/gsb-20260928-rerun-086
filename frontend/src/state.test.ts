import { describe, expect, it, vi } from 'vitest';
import { applyAuthority, applyLocalInput, initialView, loadView, saveView } from './state';
import type { AppliedInput, Snapshot } from './types';

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

describe('shared resource claims', () => {
	const pass = (remaining: number, holders: { charId: string; seq: number; tick: number }[] = []) => ({
		id: 'pass',
		name: '通行名额',
		total: 1,
		remaining,
		holders,
	});

	it('predicts a claim optimistically without moving', () => {
		const s = applyLocalInput(start(), 'c1', 1, 'claim', cfg, { resource: 'pass' });
		expect(s.pending).toHaveLength(1);
		expect(s.pending[0].resource).toBe('pass');
		expect([s.predX, s.predY]).toEqual([1, 1]);
	});

	it('records a lost arbitration with its reason and keeps it in history', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'claim', cfg, { resource: 'pass' });
		const snap: Snapshot = {
			tick: 1,
			hash: 'h-claim',
			resources: [pass(0, [{ charId: 'c2', seq: 1, tick: 1 }])],
			chars: [
				{ id: 'c1', x: 1, y: 1, facing: 'right', appliedSeq: 1 },
				{ id: 'c2', x: 4, y: 1, facing: 'left', appliedSeq: 1 },
			],
		};
		const lost: AppliedInput = {
			tick: 1, sessionId: 's1', charId: 'c1', seq: 1, cmd: 'claim', resource: 'pass',
			accepted: false, x: 1, y: 1, reason: 'resource-exhausted', ruleVersion: 1, appliedRuleVersion: 1,
		};
		s = applyAuthority(s, 'c1', snap, [lost], cfg);
		expect(s.pending).toHaveLength(0);
		expect(s.rejectedInputs).toHaveLength(1);
		expect(s.rejectedInputs[0].reason).toBe('resource-exhausted');
		expect(s.resources[0].holders[0].charId).toBe('c2');
		// A later, fully consistent snapshot must not erase the rejection.
		const snap2: Snapshot = { ...snap, tick: 2, hash: 'h2' };
		s = applyAuthority(s, 'c1', snap2, [], cfg);
		expect(s.rejectedInputs).toHaveLength(1);
	});

	it('gives a dependent action its own independent rejection reason', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'claim', cfg, { resource: 'pass' });
		s = applyLocalInput(s, 'c1', 2, 'right', cfg, { dependsOn: 1 });
		expect(s.pending[1].dependsOn).toBe(1);
		const snap: Snapshot = {
			tick: 1,
			hash: 'h-dep',
			resources: [pass(0, [{ charId: 'c2', seq: 1, tick: 1 }])],
			chars: [
				{ id: 'c1', x: 1, y: 1, facing: 'right', appliedSeq: 2 },
				{ id: 'c2', x: 4, y: 1, facing: 'left', appliedSeq: 1 },
			],
		};
		const applied: AppliedInput[] = [
			{ tick: 1, sessionId: 's1', charId: 'c1', seq: 1, cmd: 'claim', resource: 'pass', accepted: false, x: 1, y: 1, reason: 'resource-exhausted' },
			{ tick: 1, sessionId: 's1', charId: 'c1', seq: 2, cmd: 'right', dependsOn: 1, accepted: false, x: 1, y: 1, reason: 'dependency-failed' },
		];
		s = applyAuthority(s, 'c1', snap, applied, cfg);
		expect(s.rejectedInputs).toHaveLength(2);
		expect(s.rejectedInputs[1].reason).toBe('dependency-failed');
		expect(s.rejectedInputs[1].msg).toContain('seq 1');
	});

	it('marks a granted claim by folding authoritative resources', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'claim', cfg, { resource: 'pass' });
		const snap: Snapshot = {
			tick: 1,
			hash: 'h-win',
			resources: [pass(0, [{ charId: 'c1', seq: 1, tick: 1 }])],
			chars: [
				{ id: 'c1', x: 1, y: 1, facing: 'right', appliedSeq: 1 },
				{ id: 'c2', x: 4, y: 1, facing: 'left', appliedSeq: 0 },
			],
		};
		const won: AppliedInput = {
			tick: 1, sessionId: 's1', charId: 'c1', seq: 1, cmd: 'claim', resource: 'pass', accepted: true, x: 1, y: 1,
		};
		s = applyAuthority(s, 'c1', snap, [won], cfg);
		expect(s.rejectedInputs).toHaveLength(0);
		expect(s.resources[0].remaining).toBe(0);
		expect(s.resources[0].holders[0].charId).toBe('c1');
	});
});

describe('contention rounds', () => {
	const pass = (remaining: number, holders: { charId: string; seq: number; tick: number }[] = []) => ({
		id: 'pass',
		name: '通行名额',
		total: 1,
		remaining,
		holders,
	});
	const round = (over: Partial<import('./types').RoundState> = {}): import('./types').RoundState => ({
		id: 'pass-r1',
		resource: 'pass',
		state: 'collecting',
		openTick: 1,
		closeTick: 3,
		claimants: [],
		...over,
	});
	const charsAfter = (appliedSeq: number) => [
		{ id: 'c1', x: 1, y: 1, facing: 'right' as const, appliedSeq },
		{ id: 'c2', x: 4, y: 1, facing: 'left' as const, appliedSeq: 0 },
	];

	it('keeps a collected claim undecided: not confirmed, not rejected', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'claim', cfg, { resource: 'pass' });
		const snap: Snapshot = {
			tick: 1, hash: 'h-col', resources: [pass(1)], rounds: [round()], chars: charsAfter(1),
		};
		const collected: AppliedInput = {
			tick: 1, sessionId: 's1', charId: 'c1', seq: 1, cmd: 'claim', resource: 'pass',
			accepted: false, x: 1, y: 1, collected: true, roundId: 'pass-r1',
		};
		s = applyAuthority(s, 'c1', snap, [collected], cfg);
		expect(s.pending).toHaveLength(0);
		expect(s.adjudicating).toHaveLength(1);
		expect(s.adjudicating[0].roundId).toBe('pass-r1');
		expect(s.rejectedInputs).toHaveLength(0);
		expect(s.rounds[0].id).toBe('pass-r1');
	});

	it('settles a lost verdict with its reason and round identity', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'claim', cfg, { resource: 'pass' });
		s = applyAuthority(
			s, 'c1',
			{ tick: 1, hash: 'h1', resources: [pass(1)], rounds: [round()], chars: charsAfter(1) },
			[{ tick: 1, sessionId: 's1', charId: 'c1', seq: 1, cmd: 'claim', resource: 'pass', accepted: false, x: 1, y: 1, collected: true, roundId: 'pass-r1' }],
			cfg,
		);
		const verdictSnap: Snapshot = {
			tick: 3, hash: 'h3',
			resources: [pass(0, [{ charId: 'c2', seq: 1, tick: 3 }])],
			rounds: [],
			chars: charsAfter(1),
		};
		const verdict: AppliedInput = {
			tick: 3, sessionId: 's1', charId: 'c1', seq: 1, cmd: 'claim', resource: 'pass',
			accepted: false, x: 1, y: 1, derived: true, roundId: 'pass-r1', reason: 'resource-exhausted',
		};
		s = applyAuthority(s, 'c1', verdictSnap, [verdict], cfg);
		expect(s.adjudicating).toHaveLength(0);
		expect(s.rejectedInputs).toHaveLength(1);
		expect(s.rejectedInputs[0].reason).toBe('resource-exhausted');
		expect(s.rejectedInputs[0].msg).toContain('pass-r1');
	});

	it('grants a won verdict without a rejection record', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'claim', cfg, { resource: 'pass' });
		s = applyAuthority(
			s, 'c1',
			{ tick: 1, hash: 'h1', resources: [pass(1)], rounds: [round()], chars: charsAfter(1) },
			[{ tick: 1, sessionId: 's1', charId: 'c1', seq: 1, cmd: 'claim', resource: 'pass', accepted: false, x: 1, y: 1, collected: true, roundId: 'pass-r1' }],
			cfg,
		);
		s = applyAuthority(
			s, 'c1',
			{ tick: 3, hash: 'h3', resources: [pass(0, [{ charId: 'c1', seq: 1, tick: 3 }])], rounds: [], chars: charsAfter(1) },
			[{ tick: 3, sessionId: 's1', charId: 'c1', seq: 1, cmd: 'claim', resource: 'pass', accepted: true, x: 1, y: 1, derived: true, roundId: 'pass-r1' }],
			cfg,
		);
		expect(s.adjudicating).toHaveLength(0);
		expect(s.rejectedInputs).toHaveLength(0);
		expect(s.resources[0].holders[0].charId).toBe('c1');
	});

	it('defers a dependent move until the verdict, then executes it', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'claim', cfg, { resource: 'pass' });
		s = applyLocalInput(s, 'c1', 2, 'right', cfg, { dependsOn: 1 });
		s = applyAuthority(
			s, 'c1',
			{ tick: 1, hash: 'h1', resources: [pass(1)], rounds: [round()], chars: charsAfter(1) },
			[{ tick: 1, sessionId: 's1', charId: 'c1', seq: 1, cmd: 'claim', resource: 'pass', accepted: false, x: 1, y: 1, collected: true, roundId: 'pass-r1' }],
			cfg,
		);
		s = applyAuthority(
			s, 'c1',
			{ tick: 2, hash: 'h2', resources: [pass(1)], rounds: [round()], chars: charsAfter(2) },
			[{ tick: 2, sessionId: 's1', charId: 'c1', seq: 2, cmd: 'right', dependsOn: 1, accepted: false, x: 1, y: 1, deferred: true, roundId: 'pass-r1' }],
			cfg,
		);
		expect(s.adjudicating.map((p) => p.seq)).toEqual([1, 2]);
		expect(s.rejectedInputs).toHaveLength(0);
		// The deferred move's prediction stays visible while waiting.
		expect([s.predX, s.predY]).toEqual([2, 1]);
		s = applyAuthority(
			s, 'c1',
			{
				tick: 3, hash: 'h3',
				resources: [pass(0, [{ charId: 'c1', seq: 1, tick: 3 }])],
				rounds: [],
				chars: [
					{ id: 'c1', x: 2, y: 1, facing: 'right', appliedSeq: 2 },
					{ id: 'c2', x: 4, y: 1, facing: 'left', appliedSeq: 0 },
				],
			},
			[
				{ tick: 3, sessionId: 's1', charId: 'c1', seq: 1, cmd: 'claim', resource: 'pass', accepted: true, x: 1, y: 1, derived: true, roundId: 'pass-r1' },
				{ tick: 3, sessionId: 's1', charId: 'c1', seq: 2, cmd: 'right', dependsOn: 1, accepted: true, x: 2, y: 1, derived: true, roundId: 'pass-r1' },
			],
			cfg,
		);
		expect(s.adjudicating).toHaveLength(0);
		expect(s.rejectedInputs).toHaveLength(0);
		expect([s.predX, s.predY]).toEqual([2, 1]);
	});

	it('settles an adjudicating claim whose verdict was missed offline', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'claim', cfg, { resource: 'pass' });
		s = applyAuthority(
			s, 'c1',
			{ tick: 1, hash: 'h1', resources: [pass(1)], rounds: [round()], chars: charsAfter(1) },
			[{ tick: 1, sessionId: 's1', charId: 'c1', seq: 1, cmd: 'claim', resource: 'pass', accepted: false, x: 1, y: 1, collected: true, roundId: 'pass-r1' }],
			cfg,
		);
		// Room restarted; the verdict tick is gone. The round no longer
		// exists and the holder is the other client.
		s = applyAuthority(
			s, 'c1',
			{ tick: 9, hash: 'h9', resources: [pass(0, [{ charId: 'c2', seq: 1, tick: 3 }])], rounds: [], chars: charsAfter(1) },
			[],
			cfg,
		);
		expect(s.adjudicating).toHaveLength(0);
		expect(s.rejectedInputs).toHaveLength(1);
		expect(s.rejectedInputs[0].reason).toBe('settled-offline');
	});

	it('clears a closed round even when the verdict snapshot omits the rounds key', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'claim', cfg, { resource: 'pass' });
		s = applyAuthority(
			s, 'c1',
			{ tick: 1, hash: 'h1', resources: [pass(1)], rounds: [round()], chars: charsAfter(1) },
			[{ tick: 1, sessionId: 's1', charId: 'c1', seq: 1, cmd: 'claim', resource: 'pass', accepted: false, x: 1, y: 1, collected: true, roundId: 'pass-r1' }],
			cfg,
		);
		expect(s.rounds).toHaveLength(1);
		// The authoritative record for the verdict tick carries no rounds
		// key at all: the adjudicated round must not linger as collecting.
		const verdictSnap = {
			tick: 3, hash: 'h3',
			resources: [pass(0, [{ charId: 'c2', seq: 1, tick: 3 }])],
			chars: charsAfter(1),
		} as Snapshot;
		const verdict: AppliedInput = {
			tick: 3, sessionId: 's1', charId: 'c1', seq: 1, cmd: 'claim', resource: 'pass',
			accepted: false, x: 1, y: 1, derived: true, roundId: 'pass-r1', reason: 'resource-exhausted',
		};
		s = applyAuthority(s, 'c1', verdictSnap, [verdict], cfg);
		expect(s.rounds).toHaveLength(0);
		expect(s.rejectedInputs).toHaveLength(1);
		expect(s.rejectedInputs[0].reason).toBe('resource-exhausted');
	});
});
