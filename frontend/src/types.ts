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

// RoundState is the authoritative view of an open contention round: a
// persistent adjudication identity with an explicit collection boundary.
export interface RoundState {
	id: string;
	resource: string;
	state: 'collecting' | 'waiting';
	openTick: number;
	closeTick: number;
	reason?: string;
	postponements?: number;
	claimants: Holder[];
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
	config: { width: number; height: number; obstacles: [number, number][]; regions?: RegionSpec[] };
	chars: CharMeta[];
	sessions: SessionInfo[];
	rules: Rules;
	resources: ResourceSpec[];
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
	rounds?: RoundState[];
	regions?: RegionView[];
	reservations?: ReservationView[];
	hash: string;
}

// A bookable region declared by the room config.
export interface RegionSpec {
	id: string;
	name: string;
	capacity: number;
}

// Live occupancy of one region (from the authoritative snapshot).
export interface RegionView {
	id: string;
	name: string;
	capacity: number;
	used: number;
	occupants: string[];
}

// One concrete interval blocking a pending group reservation.
export interface ConflictInterval {
	kind: 'region' | 'char';
	id: string;
	start: number;
	end: number;
	by: string;
}

export type ReservationState =
	| 'pending'
	| 'approved'
	| 'active'
	| 'completed'
	| 'released'
	| 'cancelled'
	| 'rejected';

// The authoritative view of one group reservation request.
export interface ReservationView {
	id: string;
	chars: string[];
	regions: string[];
	start: number;
	end: number;
	priority: number;
	submitTick: number;
	state: ReservationState;
	reason?: string;
	conflicts?: ConflictInterval[];
	windowsWaited?: number;
	returnedTick?: number;
}

export interface ReservationResult {
	status: 'accepted' | 'duplicate';
	reservation?: ReservationView;
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
	// Collected/deferred: the input joined a contention round and is not
	// decided yet. Derived + roundId: a round verdict/execution record.
	collected?: boolean;
	deferred?: boolean;
	derived?: boolean;
	roundId?: string;
	reason?:
		| 'blocked'
		| 'sealed'
		| 'insufficient-energy'
		| 'resource-exhausted'
		| 'already-claimed'
		| 'unknown-resource'
		| 'dependency-failed';
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
	resource?: string;
	dependsOn?: number;
	sentAt: number;
	status: 'predicting' | 'adjudicating' | 'confirmed' | 'rejected' | 'duplicate' | 'forbidden' | 'error';
	predX: number;
	predY: number;
	// Rules version this input was formed under; never rewritten locally.
	ruleVersion: number;
	// Contention round this input was collected/deferred into.
	roundId?: string;
	msg?: string;
}

export interface RejectedInput extends PendingInput {
	finalX: number;
	finalY: number;
	reason: string;
	appliedRuleVersion?: number;
	needsReconfirm?: boolean;
}
