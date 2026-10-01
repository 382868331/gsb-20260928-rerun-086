import type { EnqueueResult, ReplayResult, RoomInfo, SnapshotEvent } from './types';

export interface ChaosConfig {
	// Max delay (ms) applied to every POST before it leaves the client. Each
	// input draws a random delay in [0, delayMs], reproducing reordering.
	delayMs: number;
	// Probability in [0,1] that an input is delivered twice.
	duplicateRate: number;
	// When > 0 the next send path treats the client as offline for blinkMs:
	// inputs keep queuing locally and flush (in local order) on reconnect.
	blinkMs: number;
}

export const defaultChaos: ChaosConfig = { delayMs: 0, duplicateRate: 0, blinkMs: 0 };

async function postJSON(path: string, body: unknown): Promise<{ status: number; json: unknown }> {
	const resp = await fetch(path, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify(body),
	});
	let json: unknown = null;
	try {
		json = await resp.json();
	} catch {
		// non-JSON response
	}
	return { status: resp.status, json };
}

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

type QueueResultHandler = (status: number, result: EnqueueResult | null, duplicate?: boolean) => void;

interface OutboxItem {
	seq: number;
	cmd: string;
	onResult?: QueueResultHandler;
}

export class GameClient {
	roomId: string;
	sessionId: string;
	charId: string;
	chaos: ChaosConfig = { ...defaultChaos };

	private blinkUntil = 0;
	private listeners = new Set<(ev: SnapshotEvent) => void>();
	private statusListeners = new Set<(status: TransportStatus) => void>();
	es: EventSource | null = null;
	lastEventId = -1;
	transport: TransportStatus = 'connecting';

	private outbox: OutboxItem[] = [];
	private pumping = false;

	constructor(roomId: string, sessionId: string, charId: string) {
		this.roomId = roomId;
		this.sessionId = sessionId;
		this.charId = charId;
	}

	// queueInput records an input locally and schedules delivery. Inputs may be
		// formed during a (simulated) disconnect: they sit in the FIFO outbox,
		// keep their local seq order, and flush once the client reconnects.
		queueInput(seq: number, cmd: string, onResult?: QueueResultHandler): void {
			// Defensive dedupe: the same seq must never occupy two queue slots.
			if (this.outbox.some((q) => q.seq === seq)) return;
			this.outbox.push({ seq, cmd, onResult });
			void this.pump();
		}

		// outboxLength exposes the locally buffered (not yet accepted) count.
		get outboxLength(): number {
			return this.outbox.length;
		}

		private async pump(): Promise<void> {
			if (this.pumping) return;
			this.pumping = true;
			try {
				while (this.outbox.length) {
					const item = this.outbox[0];
					const now = Date.now();
					if (this.blinkUntil > now) {
						await sleep(this.blinkUntil - now);
						continue;
					}
					if (this.chaos.delayMs > 0) {
						await sleep(Math.round(Math.random() * this.chaos.delayMs));
					}
					const body = { sessionId: this.sessionId, charId: this.charId, seq: item.seq, cmd: item.cmd };
					let delivered: { status: number; json: unknown };
					try {
						delivered = await postJSON(`/api/rooms/${this.roomId}/inputs`, body);
					} catch {
						// Network failure (POST held/failed): keep identity and
						// order, retry shortly. Server dedupe makes this safe.
						await sleep(350);
						continue;
					}
					this.outbox.shift();
					item.onResult?.(delivered.status, delivered.json as EnqueueResult);
					if (this.chaos.duplicateRate > 0 && Math.random() < this.chaos.duplicateRate) {
						void sleep(Math.max(120, Math.round(Math.random() * this.chaos.delayMs) + 200)).then(async () => {
							const d = await postJSON(`/api/rooms/${this.roomId}/inputs`, body);
							item.onResult?.(d.status, d.json as EnqueueResult, true);
						});
					}
				}
			} finally {
				this.pumping = false;
			}
		}

	// startBlink tears down SSE for blinkMs and holds outgoing POSTs, then the
	// stream resumes from the last event id (no inputs are re-run by server).
	startBlink(ms: number) {
		this.blinkUntil = Date.now() + ms;
		this.setTransport('disconnected');
		this.closeStream();
		setTimeout(() => {
			this.blinkUntil = 0;
			this.connectStream();
		}, ms);
	}

	onEvent(fn: (ev: SnapshotEvent) => void): () => void {
		this.listeners.add(fn);
		return () => this.listeners.delete(fn);
	}
	onStatus(fn: (s: TransportStatus) => void): () => void {
		this.statusListeners.add(fn);
		return () => this.statusListeners.delete(fn);
	}

	private setTransport(s: TransportStatus) {
		this.transport = s;
		this.statusListeners.forEach((fn) => fn(s));
	}

	connectStream() {
		if (this.es) return;
		const after = this.lastEventId >= 0 ? `?after=${this.lastEventId}` : '';
		const es = new EventSource(`/api/rooms/${this.roomId}/events${after}`);
		this.es = es;
		this.setTransport('connecting');
		es.onopen = () => this.setTransport('online');
		es.onmessage = (e) => {
			try {
				const ev = JSON.parse(e.data) as SnapshotEvent;
				if (ev.type === 'snapshot') {
					const id = Number(e.lastEventId);
					if (!Number.isNaN(id)) this.lastEventId = id;
					this.listeners.forEach((fn) => fn(ev));
				}
			} catch {
				// ignore malformed/heartbeat frames
			}
		};
		es.onerror = () => {
			// Native retry carries Last-Event-ID; report the degraded state.
			this.setTransport('reconnecting');
		};
	}

	closeStream() {
		this.es?.close();
		this.es = null;
	}
}

export type TransportStatus = 'connecting' | 'online' | 'reconnecting' | 'disconnected';

export async function createRoom(): Promise<RoomInfo> {
	const resp = await fetch('/api/rooms', { method: 'POST' });
	if (!resp.ok) throw new Error(`create room failed: ${resp.status}`);
	return (await resp.json()) as RoomInfo;
}

// fetchRoom resolves an existing room. The server lazily restores it from the
// journal, so a saved room survives both page reload and server restart.
export async function fetchRoom(roomId: string): Promise<RoomInfo> {
	const resp = await fetch(`/api/rooms/${roomId}`);
	if (!resp.ok) throw new Error(`room fetch failed: ${resp.status}`);
	const full = (await resp.json()) as RoomInfo & { snapshot?: unknown };
	return full;
}

export async function runReplay(roomId: string): Promise<ReplayResult> {
	const resp = await fetch(`/api/rooms/${roomId}/replay`, { method: 'POST' });
	if (!resp.ok) throw new Error(`replay failed: ${resp.status}`);
	return (await resp.json()) as ReplayResult;
}
