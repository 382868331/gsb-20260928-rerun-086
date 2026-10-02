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

export interface Rules {
	version: number;
	name: string;
	blocked?: [number, number][];
	costs?: Partial<Record<Command, number>>;
}

export interface CharState {
	id: string;
	x: number;
	y: number;
	facing: Command;
	energy?: number;
	appliedSeq: number;
	decidedSeq?: number;
}

export interface Snapshot {
	tick: number;
	ruleVersion?: number;
	rules?: Rules;
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
	decision?: 'accepted' | 'rejected' | 'needs_confirmation';
	reason?: string;
	ruleVersion?: number;
	effectiveVersion?: number;
	cost?: number;
	energy?: number;
	confirmed?: boolean;
	x: number;
	y: number;
}

export interface SnapshotEvent {
	type: 'snapshot';
	snapshot: Snapshot;
	applied: AppliedInput[];
	rejected?: { input: { sessionId: string; charId: string; seq: number; cmd: Command; ruleVersion?: number; confirmed?: boolean }; reason: string; decision?: string; effectiveVersion?: number; cost?: number; energy?: number }[];
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
	statusDetail?: 'needs_confirmation';
	ruleVersion?: number;
	originalPredX?: number;
	originalPredY?: number;
	reason?: string;
}

export interface RejectedInput extends PendingInput {
	finalX: number;
	finalY: number;
	reason: string;
}
