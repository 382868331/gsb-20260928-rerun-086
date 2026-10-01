export type Command = 'up' | 'down' | 'left' | 'right' | 'wait';

export interface CharMeta {
	id: string;
	name: string;
	color: string;
	x: number;
	y: number;
}

export interface SessionInfo {
	id: string;
	charId: string;
}

export interface RoomInfo {
	roomId: string;
	config: { width: number; height: number; obstacles: [number, number][] };
	chars: CharMeta[];
	sessions: SessionInfo[];
}

export interface CharState {
	id: string;
	x: number;
	y: number;
	facing: Command;
	appliedSeq: number;
}

export interface Snapshot {
	tick: number;
	chars: CharState[];
	hash: string;
}

export interface AppliedInput {
	tick: number;
	sessionId: string;
	charId: string;
	seq: number;
	cmd: Command;
	accepted: boolean;
}

// RefusedInput is a final authority rejection of an offline input. Its seq is
// still acknowledged (appliedSeq advances), so it can never execute later.
export interface RefusedInput {
	tick: number;
	sessionId: string;
	charId: string;
	seq: number;
	reason: 'offline-overflow' | 'offline-window' | string;
}

export interface SnapshotEvent {
	type: 'snapshot';
	snapshot: Snapshot;
	applied: AppliedInput[];
	rejected?: RefusedInput[];
}

export interface EnqueueResult {
	status: 'accepted' | 'stale' | 'forbidden';
	appliedSeq: number;
}

export interface ReplayResult {
	roomId: string;
	ticks: number;
	finalHash: string;
	liveFinalHash: string;
	match: boolean;
	divergedAt?: number;
	errors?: string[];
}

export interface PendingInput {
	seq: number;
	cmd: Command;
	sentAt: number;
	status: 'predicting' | 'confirmed' | 'rejected' | 'duplicate' | 'forbidden' | 'error' | 'queued';
	predX: number;
	predY: number;
	msg?: string;
	reason?: string;
}

// SavedClientState is persisted so a save/reload mid-session can continue
// reconciling the same pending queue, ack position and predicted layer.
export interface SavedClientState {
	version: 1;
	roomId: string;
	sessionId: string;
	charId: string;
	nextSeq: number;
	confirmedSeq: number;
	pending: PendingInput[];
	view: {
		tick: number;
		hash: string;
		predX: number;
		predY: number;
		predFacing: Command;
		auth: CharState[];
	};
	savedAt: number;
}
