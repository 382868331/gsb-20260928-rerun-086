import type { RoomInfo } from './types';

// The last room is remembered across reloads so client-side saves can be
// restored against the same authoritative room (server restores it from its
// journal on demand).

const KEY = 'gsb086:room';

function storage(): Storage | null {
	try {
		return window.localStorage;
	} catch {
		return null;
	}
}

export function saveRoom(room: RoomInfo): void {
	const st = storage();
	if (!st) return;
	try {
		st.setItem(KEY, JSON.stringify(room));
	} catch {
		// ignore
	}
}

export function loadRoom(): RoomInfo | null {
	const st = storage();
	if (!st) return null;
	try {
		const raw = st.getItem(KEY);
		return raw ? (JSON.parse(raw) as RoomInfo) : null;
	} catch {
		return null;
	}
}
