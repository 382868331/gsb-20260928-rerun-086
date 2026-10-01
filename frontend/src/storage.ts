import type { ClientViewState } from './state';
import type { SavedClientState } from './types';

// Per-room/per-session persistence. Survives a page reload (save then restore):
// the unacked queue, the last authority ack position and the predicted layer
// are all rebuilt and reconciliation continues from the live stream.

function storage(): Storage | null {
	try {
		return window.localStorage;
	} catch {
		return null;
	}
}

export function saveKey(roomId: string, sessionId: string): string {
	return `gsb086:client:${roomId}:${sessionId}`;
}

export interface SaveInput {
	roomId: string;
	sessionId: string;
	charId: string;
	nextSeq: number;
	view: ClientViewState;
}

export function saveClient(input: SaveInput): boolean {
	const st = storage();
	if (!st) return false;
	const me = input.view.auth.find((c) => c.id === input.charId);
	const data: SavedClientState = {
		version: 1,
		roomId: input.roomId,
		sessionId: input.sessionId,
		charId: input.charId,
		nextSeq: input.nextSeq,
		confirmedSeq: me?.appliedSeq ?? 0,
		pending: input.view.pending,
		view: {
			tick: input.view.tick,
			hash: input.view.hash,
			predX: input.view.predX,
			predY: input.view.predY,
			predFacing: input.view.predFacing,
			auth: input.view.auth,
		},
		savedAt: Date.now(),
	};
	try {
		st.setItem(saveKey(input.roomId, input.sessionId), JSON.stringify(data));
		return true;
	} catch {
		return false;
	}
}

export function loadClient(roomId: string, sessionId: string): SavedClientState | null {
	const st = storage();
	if (!st) return null;
	try {
		const raw = st.getItem(saveKey(roomId, sessionId));
		if (!raw) return null;
		const data = JSON.parse(raw) as SavedClientState;
		if (data.version !== 1 || data.roomId !== roomId || data.sessionId !== sessionId) return null;
		return data;
	} catch {
		return null;
	}
}

export function clearClient(roomId: string, sessionId: string): void {
	const st = storage();
	if (!st) return;
	try {
		st.removeItem(saveKey(roomId, sessionId));
	} catch {
		// ignore
	}
}
