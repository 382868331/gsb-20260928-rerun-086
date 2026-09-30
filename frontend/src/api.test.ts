import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { GameClient } from './api';

class FakeES {
	static last: FakeES | null = null;
	onopen: (() => void) | null = null;
	onmessage: ((e: { data: string; lastEventId?: string }) => void) | null = null;
	onerror: (() => void) | null = null;
	url: string;
	constructor(url: string) {
		this.url = url;
		FakeES.last = this;
	}
	close() {}
}

describe('GameClient blackout drill', () => {
	beforeEach(() => {
		(globalThis as unknown as { EventSource: unknown }).EventSource = FakeES;
	});
	afterEach(() => {
		vi.restoreAllMocks();
	});

	it('holds inputs during a disconnect and flushes them with the same seq after resume', async () => {
		const posts: unknown[] = [];
		vi.stubGlobal('fetch', vi.fn(async (_url: string, init?: { body?: string }) => {
			posts.push(JSON.parse(init?.body ?? '{}'));
			return { ok: true, status: 202, json: async () => ({ status: 'accepted', appliedSeq: 1 }) };
		}));
		const client = new GameClient('room1', 's1', 'c1');
		const statuses: string[] = [];
		client.onStatus((s) => statuses.push(s));
		client.connectStream();
		FakeES.last!.onopen!();

		const queuedSeen: number[][] = [];
		client.onQueue((q) => queuedSeen.push(q.map((i) => i.seq)));

		client.startBlink(120);
		const p1 = client.sendInput(1, 'right');
		await new Promise((r) => setTimeout(r, 30));
		expect(posts).toHaveLength(0, );
		expect(queuedSeen.at(-1)).toEqual([1]);

		// Stream comes back; resume signals online, queued input goes out once.
		client.connectStream();
		FakeES.last!.onopen!();
		const res = await p1;
		expect(res.result?.status).toBe('accepted');
		expect(posts).toHaveLength(1);
		expect((posts[0] as { seq: number }).seq).toBe(1);
		expect(queuedSeen.at(-1)).toEqual([]);
		expect(statuses).toContain('disconnected');
		expect(statuses.at(-1)).toBe('online');
		client.closeStream();
	});
});
