export type Command = 'up' | 'down' | 'left' | 'right' | 'wait' | 'claim';

// Shared finite resource clients contend for (pass slot, one-time item).
export interface ResourceSpec {
	id: string;
	name: string;
	total: number;
}

export interface Holder {
	charId: string;
	seq: number;
	tick: number;
}

export interface ResourceState {
	id: string;
	name: string;
	total: number;
	remaining: number;
	holders: Holder[];
}

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
	resources: ResourceSpec[];
	rounds?: RoundView[];
}

// Shared adjudication view of one contention round, identical for every
// client: who has been collected, who is still missing, and the verdict.
export interface RoundView {
	id: string;
	resource: string;
	status: 'open' | 'adjudicated' | 'cancelled';
	openTick: number;
	deadlineMs: number;
	closeTick?: number;
	closeReason?: 'complete' | 'deadline' | 'manual';
	claims: { charId: string; seq: number; ruleVersion: number }[];
	waitingFor: string[];
	missing?: string[];
	winner?: string;
}

// Rules is one versioned set of action rules decided by the authority.
export interface Rules {
	version: number;
	sealed?: [number, number][];
	costs: Partial<Record<Command, number>>;
	claimsClosed?: boolean;
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
	resources?: ResourceState[];
	hash: string;
}

export interface AppliedInput {
	tick: number;
	sessionId: string;
	charId: string;
	seq: number;
	cmd: Command;
	resource?: string;
	dependsOn?: number;
	accepted: boolean;
	x: number;
	y: number;
	// Basis declared by the client vs. version the authority applied.
	ruleVersion?: number;
	appliedRuleVersion?: number;
	// Contention round this claim was adjudicated in.
	roundId?: string;
	reason?:
		| 'blocked'
		| 'sealed'
		| 'insufficient-energy'
		| 'resource-exhausted'
		| 'already-claimed'
		| 'unknown-resource'
		| 'dependency-failed'
		| 'round-cancelled';
	needsReconfirm?: boolean;
}

export interface SnapshotEvent {
	type: 'snapshot';
	snapshot: Snapshot;
	applied: AppliedInput[];
	rejected?: AppliedInput[];
	rules?: Rules;
	rounds?: RoundView[];
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
	resource?: string;
	dependsOn?: number;
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
