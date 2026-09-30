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

export interface SnapshotEvent {
	type: 'snapshot';
	snapshot: Snapshot;
	applied: AppliedInput[];
	rejected?: { input: { sessionId: string; charId: string; seq: number; cmd: Command }; reason: string }[];
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
	status: 'predicting' | 'confirmed' | 'rejected' | 'duplicate' | 'forbidden' | 'error';
	predX: number;
	predY: number;
	msg?: string;
}

// QueuedInput is an input held locally while the link is down. It is not
// dropped or optimistically retried; after reconnect it is delivered once
// with the same seq.
export interface QueuedInput {
	seq: number;
	cmd: Command;
	queuedAt: number;
}
