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
