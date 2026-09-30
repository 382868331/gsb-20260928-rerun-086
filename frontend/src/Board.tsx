import { Fragment } from 'react';
import type { CharMeta, CharState, Command } from './types';

interface BoardProps {
	width: number;
	height: number;
	obstacles: [number, number][];
	chars: CharMeta[];
	states: CharState[];
	localCharId: string;
	predX: number;
	predY: number;
	predFacing: Command;
	hasPending: boolean;
	lastCorrectionAt: number | null;
}

const arrow: Record<Command, string> = {
	up: '▲',
	down: '▼',
	left: '◀',
	right: '▶',
	wait: '•',
};

export function Board(props: BoardProps) {
	const { width, height, obstacles, chars, states } = props;
	const wall = new Set(obstacles.map(([x, y]) => `${x},${y}`));
	const cells = [];
	for (let y = 0; y < height; y++) {
		for (let x = 0; x < width; x++) {
			cells.push(
				<div key={`${x},${y}`} className={`cell ${wall.has(`${x},${y}`) ? 'wall' : ''}`} />,
			);
		}
	}
	return (
		<div
			className="board"
			style={{ gridTemplateColumns: `repeat(${width}, 1fr)`, aspectRatio: `${width} / ${height}` }}
		>
			{cells}
			{chars.map((meta) => {
				const isLocal = meta.id === props.localCharId;
				const st = states.find((s) => s.id === meta.id);
				if (!st) return null;
				const predicted = isLocal && props.hasPending;
				const corrected = !!(isLocal && props.lastCorrectionAt && Date.now() - props.lastCorrectionAt < 900);
				const marker = (x: number, y: number, facing: Command, kind: 'confirmed' | 'predicted') => (
					<div
						key={`${meta.id}-${kind}`}
						className={`char ${kind} ${kind === 'confirmed' && predicted ? 'ghost' : ''} ${kind === 'confirmed' && corrected ? 'corrected' : ''}`}
						style={{
							left: `${(x / width) * 100}%`,
							top: `${(y / height) * 100}%`,
							width: `${100 / width}%`,
							height: `${100 / height}%`,
							background: meta.color,
						}}
						title={kind === 'predicted' ? '本地预测位置（未经服务端确认）' : '服务端已确认位置（最后权威快照）'}
					>
						<span>{arrow[facing]}</span>
						<label>{meta.name}{kind === 'predicted' ? ' · 预测' : ''}</label>
					</div>
				);
				if (!predicted) return marker(st.x, st.y, st.facing, 'confirmed');
				// While inputs are unacked, draw BOTH layers: the solid ghost
				// stays at the last confirmed cell, the dashed marker shows
				// the predicted cell. They coincide when the prediction so
				// far agrees with the last authority (e.g. waiting).
				return (
					<Fragment key={meta.id}>
						{marker(st.x, st.y, st.facing, 'confirmed')}
						{marker(props.predX, props.predY, props.predFacing, 'predicted')}
					</Fragment>
				);
			})}
		</div>
	);
}
