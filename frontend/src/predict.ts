import type { CharState, Command } from './types';

// Predicted view mirrors the authoritative movement rules in the Go engine
// (bounds, static walls, other characters). It exists only to render the
// local player's own inputs early; every snapshot from the server replaces
// it, and divergence is corrected instead of being smoothed away.
export interface PredConfig {
	width: number;
	height: number;
	obstacles: [number, number][];
	// Cells temporarily sealed by the current rules version.
	sealed?: [number, number][];
}

export function step(
	x: number,
	y: number,
	cmd: Command,
	others: { x: number; y: number }[],
	cfg: PredConfig,
): { x: number; y: number; moved: boolean } {
	// A claim does not move the character; its outcome (slot granted or
	// conflict) is decided by the authority, not by local prediction.
	if (cmd === 'claim') return { x, y, moved: false };
	let nx = x;
	let ny = y;
	if (cmd === 'up') ny--;
	else if (cmd === 'down') ny++;
	else if (cmd === 'left') nx--;
	else if (cmd === 'right') nx++;
	if (nx < 0 || ny < 0 || nx >= cfg.width || ny >= cfg.height) return { x, y, moved: false };
	if (cfg.obstacles.some(([ox, oy]) => ox === nx && oy === ny)) return { x, y, moved: false };
	if (cfg.sealed?.some(([sx, sy]) => sx === nx && sy === ny)) return { x, y, moved: false };
	if (others.some((o) => o.x === nx && o.y === ny)) return { x, y, moved: false };
	return { x: nx, y: ny, moved: true };
}

// reconcile reports whether the authoritative position differs from what the
// client predicted for the given confirmed input. The UI must snap to the
// server value instead of interpolating.
export function reconcile(pred: { x: number; y: number }, authority: CharState): boolean {
	return pred.x !== authority.x || pred.y !== authority.y;
}
