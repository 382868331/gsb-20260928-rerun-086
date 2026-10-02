import { describe, expect, it, vi } from 'vitest';
import { applyAuthority, applyLocalInput, initialView, loadView, saveView } from './state';
import type { AppliedInput, Snapshot } from './types';

function applied(
	seq: number,
	accepted: boolean,
	x: number,
	y: number,
	charId = 'c1',
	cmd: AppliedInput['cmd'] = 'right',
	decision: AppliedInput['decision'] = accepted ? 'accepted' : undefined,
	ruleVersion = 1,
	confirmed = false,
): AppliedInput {
	return { tick: 1, sessionId: 's1', charId, seq, cmd, accepted, x, y, decision, ruleVersion, confirmed };
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

	it('preserves an old-rule prediction for confirmation while processing later inputs independently', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'up', { ...cfg, costs: { up: 3 } });
		s = applyLocalInput(s, 'c1', 2, 'wait', { ...cfg, costs: { up: 3 } });
		const snap: Snapshot = {
			tick: 1,
			hash: 'rule-v2',
			ruleVersion: 2,
			rules: { version: 2, name: 'v2', costs: { up: 3, wait: 0 } },
			chars: [
				{ id: 'c1', x: 2, y: 2, facing: 'right', energy: 10, appliedSeq: 2, decidedSeq: 2 },
				{ id: 'c2', x: 4, y: 1, facing: 'left', energy: 10, appliedSeq: 0 },
			],
		};
		s = applyAuthority(
			s,
			'c1',
			snap,
			[
				{ ...applied(1, false, 2, 2), decision: 'needs_confirmation', reason: 'rule-version-changed', ruleVersion: 1, effectiveVersion: 2, cmd: 'up' },
				{ ...applied(2, true, 2, 2), cmd: 'wait' },
			],
			{ ...cfg, costs: { up: 3, wait: 0 } },
		);
		expect(s.pending.map((p) => p.seq)).toEqual([1]);
		expect(s.pending[0].statusDetail).toBe('needs_confirmation');
		expect([s.pending[0].predX, s.pending[0].predY]).toEqual([1, 0]);

		const lateSnap: Snapshot = {
			tick: 2,
			hash: 'confirmed',
			ruleVersion: 2,
			rules: { version: 2, name: 'v2', costs: { up: 3, wait: 0 } },
			chars: [
				{ id: 'c1', x: 2, y: 1, facing: 'up', energy: 7, appliedSeq: 2, decidedSeq: 2 },
				{ id: 'c2', x: 4, y: 1, facing: 'left', energy: 10, appliedSeq: 0 },
			],
		};
		s = applyAuthority(s, 'c1', lateSnap, [{ ...applied(1, true, 2, 1, 'c1', 'up', 'accepted', 1, true) }], { ...cfg, costs: { up: 3 } });
		expect(s.pending).toHaveLength(0);
		expect([s.predX, s.predY]).toEqual([2, 1]);
		expect(s.predEnergy).toBe(7);
	});
});
