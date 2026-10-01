// @vitest-environment jsdom
import { describe, expect, it } from 'vitest';
import { applyAuthority, applyLocalInput, initialView, restoreView } from './state';
import { loadClient, saveClient } from './storage';
import type { SavedClientState, Snapshot } from './types';

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
		s = applyAuthority(s, 'c1', snap, [], cfg);
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
		s = applyAuthority(s, 'c1', snap, [], cfg);
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
		s = applyAuthority(s, 'c1', snap, [], cfg);
		expect(s.pending.map((p) => p.seq)).toEqual([2]);
		expect([s.predX, s.predY]).toEqual([3, 1]);
	});

	it('marks a server-refused offline input for review but keeps later inputs ordered', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'right', cfg);
		s = applyLocalInput(s, 'c1', 2, 'right', cfg);
		s = applyLocalInput(s, 'c1', 3, 'up', cfg);
		// Authority refuses seq 2 (offline-overflow) but executes seq 1; seq 3
		// is beyond the ack and survives in the queue, preserving local order.
		const snap: Snapshot = {
			tick: 1,
			hash: 'h-ref',
			chars: [
				{ id: 'c1', x: 2, y: 1, facing: 'right', appliedSeq: 2 },
				{ id: 'c2', x: 4, y: 1, facing: 'left', appliedSeq: 0 },
			],
		};
		s = applyAuthority(s, 'c1', snap, [
			{ tick: 1, sessionId: 's1', charId: 'c1', seq: 2, reason: 'offline-overflow' },
		], cfg);
		expect(s.pending.map((p) => p.seq)).toEqual([3]);
		const refused = s.reviewed.filter((p) => p.status === 'rejected');
		expect(refused).toHaveLength(1);
		expect(refused[0].seq).toBe(2);
		expect(refused[0].predX).toBe(3);
		expect(refused[0].reason).toBe('offline-overflow');
		// Survivor 'up' targets wall (2,0): prediction re-applies the rule and
		// stays blocked at (2,1), proving the tail was replayed, not reordered.
		expect([s.predX, s.predY]).toEqual([2, 1]);
	});

	it('persists and restores the pending queue, ack position and prediction', () => {
		let s = applyLocalInput(start(), 'c1', 1, 'right', cfg);
		s = applyLocalInput(s, 'c1', 2, 'right', cfg);
		const snap: Snapshot = {
			tick: 5,
			hash: 'persisted',
			chars: [
				{ id: 'c1', x: 2, y: 1, facing: 'right', appliedSeq: 1 },
				{ id: 'c2', x: 4, y: 1, facing: 'left', appliedSeq: 0 },
			],
		};
		s = applyAuthority(s, 'c1', snap, [], cfg);
		const ok = saveClient({ roomId: 'room', sessionId: 's1', charId: 'c1', nextSeq: 3, view: s });
		expect(ok).toBe(true);
		const saved = loadClient('room', 's1') as SavedClientState;
		expect(saved.nextSeq).toBe(3);
		expect(saved.confirmedSeq).toBe(1);
		expect(saved.pending.map((p) => p.seq)).toEqual([2]);
		const restored = restoreView(saved);
		expect(restored.tick).toBe(5);
		expect(restored.pending.map((p) => p.seq)).toEqual([2]);
		expect([restored.predX, restored.predY]).toEqual([3, 1]);
	});
});
