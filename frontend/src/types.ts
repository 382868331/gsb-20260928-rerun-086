export type Command = 'up' | 'down' | 'left' | 'right' | 'wait' | 'claim';

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
	config: { width: number; height: number; obstacles: [number, number][]; passes: number };
	chars: CharMeta[];
	sessions: SessionInfo[];
	rules: Rules;
}

// Rules is one versioned set of action rules decided by the authority.
export interface Rules {
	version: number;
	sealed?: [number, number][];
	costs: Partial<Record<Command, number>>;
	// When true, the authority rejects shared-resource claims under this
	// version (old-basis claims get an explicit re-confirm request).
	claimsClosed?: boolean;
}

export interface CharState {
	id: string;
	x: number;
	y: number;
	facing: Command;
	energy?: number;
	// Shared-resource slots this character holds.
	passes?: number;
	appliedSeq: number;
}

export interface Snapshot {
	tick: number;
	chars: CharState[];
	// Remaining shared-resource pool; part of the authoritative state.
	passesLeft?: number;
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
	// Seq of the same-session input this one depended on (0 = independent).
	dependsOn?: number;
	reason?: 'blocked' | 'sealed' | 'insufficient-energy' | 'resource-exhausted' | 'dependency-rejected' | 'claims-closed';
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
	// Seq of the earlier local action this one depends on (0 = independent).
	dependsOn?: number;
	msg?: string;
}

export interface RejectedInput extends PendingInput {
	finalX: number;
	finalY: number;
	reason: string;
	appliedRuleVersion?: number;
	needsReconfirm?: boolean;
}

// ClaimVerdict is the authority's adjudication of one shared-resource
// claim, shown identically on every client: who actually got the slot and
// why the others were rejected.
export interface ClaimVerdict {
	tick: number;
	charId: string;
	seq: number;
	granted: boolean;
	reason?: string;
	ruleVersion?: number;
	appliedRuleVersion?: number;
}
