import type { CharMeta, CharState, Command } from './types';

interface BoardProps {
	width: number;
	height: number;
	obstacles: [number, number][];
	sealed: [number, number][];
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
	claim: '★',
};

export function Board(props: BoardProps) {
	const { width, height, obstacles, sealed, chars, states } = props;
	const wall = new Set(obstacles.map(([x, y]) => `${x},${y}`));
	const sealedSet = new Set(sealed.map(([x, y]) => `${x},${y}`));
	const cells = [];
	for (let y = 0; y < height; y++) {
		for (let x = 0; x < width; x++) {
			const key = `${x},${y}`;
			cells.push(
				<div
					key={key}
					className={`cell ${wall.has(key) ? 'wall' : ''} ${sealedSet.has(key) ? 'sealed' : ''}`}
					title={sealedSet.has(key) ? '当前规则下暂时封闭' : undefined}
				/>,
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
				const x = predicted ? props.predX : st.x;
				const y = predicted ? props.predY : st.y;
				const facing = predicted ? props.predFacing : st.facing;
				const corrected = isLocal && props.lastCorrectionAt && Date.now() - props.lastCorrectionAt < 900;
				return (
					<div
						key={meta.id}
						className={`char ${predicted ? 'predicted' : 'confirmed'} ${corrected ? 'corrected' : ''}`}
						style={{
							left: `${(x / width) * 100}%`,
							top: `${(y / height) * 100}%`,
							width: `${100 / width}%`,
							height: `${100 / height}%`,
							background: meta.color,
						}}
						title={predicted ? '本地预测位置（未经服务端确认）' : '服务端确认位置'}
					>
						<span>{arrow[facing]}</span>
						<label>{meta.name}{(st.passes ?? 0) > 0 ? ' ★' : ''}</label>
					</div>
				);
			})}
		</div>
	);
}
