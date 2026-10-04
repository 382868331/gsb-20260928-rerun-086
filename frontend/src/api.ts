import type { Command, EnqueueResult, ReplayResult, RoomInfo, Rules, SnapshotEvent } from './types';

export interface ChaosConfig {
	// Delay range (ms) applied to every POST before it leaves the client.
	delayMs: number;
	// Probability in [0,1] that an input is delivered twice.
	duplicateRate: number;
	// When true, the client simulates a short disconnect: POSTs are held and
	// the event stream is torn down, then everything reconnects/resumes.
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

export class GameClient {
	roomId: string;
	sessionId: string;
	charId: string;
	chaos: ChaosConfig = { ...defaultChaos };

	private blinkUntil = 0;
	private sendChain: Promise<void> = Promise.resolve();
	private listeners = new Set<(ev: SnapshotEvent) => void>();
	private statusListeners = new Set<(status: TransportStatus) => void>();
	es: EventSource | null = null;
	lastEventId = -1;
	lastTick = 0;
	transport: TransportStatus = 'connecting';

	constructor(roomId: string, sessionId: string, charId: string) {
		this.roomId = roomId;
		this.sessionId = sessionId;
		this.charId = charId;
	}

	// sendInput applies client-side chaos before the authoritative POST.
	// Returns the primary result; a duplicated redelivery result is reported
	// via onDuplicate so the UI can show the server's stale ack.
	async sendInput(
		seq: number,
		cmd: string,
		ruleVersion: number,
		onDuplicate?: (status: number, result: EnqueueResult) => void,
		extra?: { resource?: string; dependsOn?: number },
	): Promise<{ status: number; result: EnqueueResult | null; heldMs: number }> {
		let resolveCall!: (v: { status: number; result: EnqueueResult | null; heldMs: number }) => void;
		let rejectCall!: (err: unknown) => void;
		const call = new Promise<{ status: number; result: EnqueueResult | null; heldMs: number }>((resolve, reject) => {
			resolveCall = resolve;
			rejectCall = reject;
		});
		const run = this.sendChain.then(async () => {
			const body = {
				sessionId: this.sessionId,
				charId: this.charId,
				seq,
				cmd,
				ruleVersion,
				resource: extra?.resource,
				dependsOn: extra?.dependsOn ?? 0,
			};
			try {
				const heldMs = await this.weatherBlackout();
				if (this.chaos.delayMs > 0) await sleep(this.chaos.delayMs);
				const primary = postJSON(`/api/rooms/${this.roomId}/inputs`, body);
				let duplicate: Promise<void> | null = null;
				if (this.chaos.duplicateRate > 0 && Math.random() < this.chaos.duplicateRate) {
					duplicate = sleep(Math.max(120, this.chaos.delayMs + 200)).then(async () => {
						const d = await postJSON(`/api/rooms/${this.roomId}/inputs`, body);
						onDuplicate?.(d.status, d.json as EnqueueResult);
					});
				}
				const { status, json } = await primary;
				await duplicate;
				resolveCall({ status, result: json as EnqueueResult, heldMs });
			} catch (err) {
				rejectCall(err);
			}
		});
		this.sendChain = run.catch(() => undefined);
		return call;
	}

	private async weatherBlackout(): Promise<number> {
	const now = Date.now();
	const waitFor = this.blinkUntil - now;
		if (waitFor <= 0) return 0;
		await sleep(waitFor);
		return waitFor;
	}

	// startBlink tears down SSE for blinkMs and serializes outgoing POSTs,
	// then the stream resumes from the durable authority tick.
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
		const after = this.lastTick > 0 ? `?afterTick=${this.lastTick}` : '';
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
					this.lastTick = Math.max(this.lastTick, ev.snapshot.tick);
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

// getRoom rejoins an existing room (after a page refresh or a server
// restart); throws when the room no longer exists.
export async function getRoom(roomId: string): Promise<RoomInfo> {
	const resp = await fetch(`/api/rooms/${roomId}`);
	if (!resp.ok) throw new Error(`get room failed: ${resp.status}`);
	return (await resp.json()) as RoomInfo;
}

// cancelRound explicitly cancels an open contention round: collected
// claims are refused with "round-cancelled" instead of being silently
// decided by arrival order.
export async function cancelRound(roomId: string, roundId: string): Promise<void> {
	const resp = await fetch(`/api/rooms/${roomId}/rounds/${encodeURIComponent(roundId)}/cancel`, { method: 'POST' });
	if (!resp.ok) throw new Error(`cancel round failed: ${resp.status}`);
}

export async function runReplay(roomId: string): Promise<ReplayResult> {
	const resp = await fetch(`/api/rooms/${roomId}/replay`, { method: 'POST' });
	if (!resp.ok) throw new Error(`replay failed: ${resp.status}`);
	return (await resp.json()) as ReplayResult;
}

// changeRules asks the authority to switch to a new rules version (sealed
// region and/or action costs). Returns the rules now in force.
export async function changeRules(
	roomId: string,
	sealed: [number, number][],
	costs: Partial<Record<Command, number>>,
	claimsClosed?: boolean,
): Promise<Rules> {
	const resp = await fetch(`/api/rooms/${roomId}/rules`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ sealed, costs, claimsClosed }),
	});
	if (!resp.ok) throw new Error(`rules switch failed: ${resp.status}`);
	return (await resp.json()) as Rules;
}
