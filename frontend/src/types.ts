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
	rules: Rules;
}

// Rules is one versioned set of action rules decided by the authority.
export interface Rules {
	version: number;
	sealed?: [number, number][];
	costs: Partial<Record<Command, number>>;
}

export interface CharState {
	id: string;
	x: number;
	y: number;
	facing: Command;
	energy?: number;
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
	x: number;
	y: number;
	// Basis declared by the client vs. version the authority applied.
	ruleVersion?: number;
	appliedRuleVersion?: number;
	reason?: 'blocked' | 'sealed' | 'insufficient-energy';
	needsReconfirm?: boolean;
}

export interface SnapshotEvent {
	type: 'snapshot';
	snapshot: Snapshot;
	applied: AppliedInput[];
	rejected?: AppliedInput[];
	rules?: Rules;
}

export interface EnqueueResult {
	status: 'accepted' | 'stale' | 'forbidden';
	appliedSeq: number;
	ruleVersion?: number;
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
	// Rules version this input was formed under; never rewritten locally.
	ruleVersion: number;
	msg?: string;
}

export interface RejectedInput extends PendingInput {
	finalX: number;
	finalY: number;
	reason: string;
	appliedRuleVersion?: number;
	needsReconfirm?: boolean;
}
