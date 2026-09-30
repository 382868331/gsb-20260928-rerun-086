// @vitest-environment jsdom

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, findByText, waitFor } from '@testing-library/react';

type ESHandler = (ev: { data: string; lastEventId?: string }) => void;

class FakeES {
	static all: FakeES[] = [];
	url: string;
	onopen: (() => void) | null = null;
	onmessage: ESHandler | null = null;
	onerror: (() => void) | null = null;
	constructor(url: string) {
		this.url = url;
		FakeES.all.push(this);
	}
	close() {}
}

const chars = () => [
	{ id: 'c1', name: '角色 A', color: '#38bdf8', x: 2, y: 2 },
	{ id: 'c2', name: '角色 B', color: '#fb7185', x: 15, y: 7 },
];

describe('App end-to-end render resilience', () => {
	beforeEach(() => {
		FakeES.all = [];
		(globalThis as unknown as { EventSource: unknown }).EventSource = FakeES;
	});

	it('keeps the shell usable with no room and renders two panels after creation', async () => {
		vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 503, json: async () => ({}) })));
		const { default: App } = await import('./App');
		const view = render(<App />);
		// No white screen: shell, controls and placeholders stay operable.
		expect(await findByText(view.container, '尚无活动房间')).toBeTruthy();
		expect(await findByText(view.container, '重试创建房间')).toBeTruthy();
		expect(view.container.textContent).toContain('客户端 A：等待房间');

		// Recover after retry.
		vi.stubGlobal('fetch', vi.fn(async (input: unknown) => {
			if (String(input).endsWith('/api/rooms')) {
				return {
					ok: true, status: 201,
					json: async () => ({
						roomId: 'room1',
						config: { width: 18, height: 10, obstacles: [] },
						chars: chars(),
						sessions: [{ id: 's1', charId: 'c1' }, { id: 's2', charId: 'c2' }],
					}),
				};
			}
			return { ok: true, status: 200, json: async () => ({ status: 'accepted' }) };
		}));
		fireEvent.click(await findByText(view.container, '重试创建房间'));
		expect(await findByText(view.container, '角色 A · 本地客户端 1')).toBeTruthy();
		expect(await findByText(view.container, '角色 B · 本地客户端 2')).toBeTruthy();

		// Heartbeat snapshots with empty applied lists must not crash.
		FakeES.all.forEach((es, i) => {
			es.onopen?.();
			es.onmessage?.({ data: JSON.stringify({ type: 'snapshot', snapshot: { tick: 1, hash: 'h', chars: [
				{ id: 'c1', x: 2, y: 2, facing: 'right', appliedSeq: 0 },
				{ id: 'c2', x: 15, y: 7, facing: 'left', appliedSeq: 0 },
			] }, applied: [] }), lastEventId: String(i + 1) });
		});
		await waitFor(() => expect(view.container.textContent).toContain('权威 tick: 1'));
	});
});
