import { useEffect, useRef, useState } from 'react';
import { changeRules, createRoom, getRoom, runReplay, type ChaosConfig, defaultChaos } from './api';
import { ClientPanel } from './ClientPanel';
import type { Command, ReplayResult, RoomInfo } from './types';

// Demo rule presets: seal a corridor and/or raise movement cost.
const SEAL_REGION: [number, number][] = [
	[6, 3], [6, 4], [6, 5], [6, 6], [7, 4], [7, 5],
];
const NORMAL_COSTS: Record<Command, number> = { up: 1, down: 1, left: 1, right: 1, wait: 0, claim: 0 };
const COSTLY_COSTS: Record<Command, number> = { up: 5, down: 5, left: 5, right: 5, wait: 0, claim: 0 };

interface LogLine {
	id: number;
	text: string;
	kind: 'info' | 'warn' | 'ok';
	at: number;
}

const ROOM_STORAGE_KEY = 'gameplatform:room';

export default function App() {
	const [room, setRoom] = useState<RoomInfo | null>(null);
	const [error, setError] = useState<string>('');
	const [chaos, setChaos] = useState<ChaosConfig>({ ...defaultChaos });
	const [logs, setLogs] = useState<LogLine[]>([]);
	const [replay, setReplay] = useState<ReplayResult | null>(null);
	const logId = useRef(0);
	const [autoReplay, setAutoReplay] = useState(false);

	function pushLog(text: string, kind: LogLine['kind'] = 'info') {
		logId.current += 1;
		const line = { id: logId.current, text, kind, at: Date.now() };
		setLogs((prev) => [...prev.slice(-80), line]);
	}

	useEffect(() => {
		// Refresh restores the previous room and sessions instead of
		// abandoning them: unconfirmed predictions and the recovery queue
		// keep their identity and continue to settle against the same room.
		const savedRoomId = localStorage.getItem(ROOM_STORAGE_KEY);
		const boot = savedRoomId
			? getRoom(savedRoomId).catch(() => {
					localStorage.removeItem(ROOM_STORAGE_KEY);
					return createRoom();
				})
			: createRoom();
		boot
			.then((info) => {
				localStorage.setItem(ROOM_STORAGE_KEY, info.roomId);
				setRoom(info);
				pushLog(
					savedRoomId === info.roomId
						? `已恢复原房间：${info.roomId}（会话与待确认队列继续核对）`
						: `房间已创建：${info.roomId}（两个角色、两个会话）`,
					'ok',
				);
			})
			.catch((e) => setError(`无法连接 Go 权威服务：${String(e)}`));
	}, []);

	function newRoom() {
		// Abandon the current room: clear every persisted view and reboot.
		for (const key of Object.keys(localStorage)) {
			if (key.startsWith('gameplatform:')) localStorage.removeItem(key);
		}
		location.reload();
	}

	useEffect(() => {
		if (!autoReplay || !room) return;
		const id = setInterval(() => {
			runReplay(room.roomId).then(setReplay).catch(() => undefined);
		}, 4000);
		return () => clearInterval(id);
	}, [autoReplay, room]);

	async function doReplay() {
		if (!room) return;
		const r = await runReplay(room.roomId);
		setReplay(r);
		pushLog(
			r.match
				? `重放 ${r.ticks} 个 tick，最终 hash 与实时记录一致`
				: `重放从 tick ${r.divergedAt} 起与实时记录不一致`,
			r.match ? 'ok' : 'warn',
		);
	}

	async function switchRules(sealed: [number, number][], costs: Record<Command, number>, label: string, claimsClosed?: boolean) {
		if (!room) return;
		try {
			const rules = await changeRules(room.roomId, sealed, costs, claimsClosed);
			pushLog(
				`规则切换为 v${rules.version}（${label}）：封闭 ${rules.sealed?.length ?? 0} 格，移动消耗 ${rules.costs.right ?? 1}${rules.claimsClosed ? '，资源领取已关闭' : ''}；旧依据输入将由服务端逐项判定`,
				'warn',
			);
		} catch (e) {
			pushLog(`规则切换失败：${String(e)}`, 'warn');
		}
	}

	if (error) {
		return <div className="fatal">{error}<br />请先启动 Go 服务：<code>go run ./cmd/server</code></div>;
	}
	if (!room) return <div className="fatal">正在创建权威房间…</div>;

	return (
		<div className="app">
			<header className="topbar">
				<h1>游戏服务研发平台 · 虚拟角色状态同步</h1>
				<div className="room mono small">room {room.roomId}</div>
				<button className="new-room" onClick={newRoom}>新建房间（放弃当前）</button>
			</header>

			<section className="controls">
				<div className="control-group">
					<label>消息迟到延迟 ms</label>
					<input
						type="range" min={0} max={1500} step={50}
						value={chaos.delayMs}
						onChange={(e) => setChaos({ ...chaos, delayMs: Number(e.target.value) })}
					/>
					<b>{chaos.delayMs}</b>
				</div>
				<div className="control-group">
					<label>重复送达概率</label>
					<input
						type="range" min={0} max={1} step={0.1}
						value={chaos.duplicateRate}
						onChange={(e) => setChaos({ ...chaos, duplicateRate: Number(e.target.value) })}
					/>
					<b>{Math.round(chaos.duplicateRate * 100)}%</b>
				</div>
				<div className="control-group">
					<label>断开持续 ms（手动按钮使用）</label>
					<input
						type="range" min={500} max={6000} step={100}
						value={chaos.blinkMs || 2500}
						onChange={(e) => setChaos({ ...chaos, blinkMs: Number(e.target.value) })}
					/>
					<b>{chaos.blinkMs || 2500}</b>
				</div>
				<div className="control-group replay-group">
					<button onClick={doReplay}>从保存事件重放校验</button>
					<label className="inline">
						<input type="checkbox" checked={autoReplay} onChange={(e) => setAutoReplay(e.target.checked)} />
						每 4 秒自动校验
					</label>
				</div>
				<div className="control-group replay-group">
					<button onClick={() => switchRules(SEAL_REGION, NORMAL_COSTS, '暂时封闭中部区域')}>封闭一段区域</button>
					<button onClick={() => switchRules([], COSTLY_COSTS, '移动消耗 ×5')}>提高移动消耗</button>
					<button onClick={() => switchRules([], NORMAL_COSTS, '关闭资源领取', true)}>关闭资源领取</button>
					<button onClick={() => switchRules([], NORMAL_COSTS, '恢复默认规则')}>恢复默认规则</button>
				</div>
			</section>

			{replay && (
				<section className={`replay ${replay.match ? 'ok' : 'bad'}`}>
					<div>重放 tick 数：<b>{replay.ticks}</b></div>
					<div>实时 hash：<code>{replay.liveFinalHash.slice(0, 20)}</code></div>
					<div>重放 hash：<code>{replay.finalHash.slice(0, 20)}</code></div>
					<div className="verdict">{replay.match ? '✓ 重放结果与服务端实时结论完全一致' : `✗ 在 tick ${replay.divergedAt} 出现分歧`}</div>
				</section>
		)}

			<section className="panels">
				<ClientPanel room={room} sessionIndex={0} chaos={chaos} duplicateSeen={(t) => pushLog(t, 'warn')} />
				<ClientPanel room={room} sessionIndex={1} chaos={chaos} duplicateSeen={(t) => pushLog(t, 'warn')} />
			</section>

			<section className="journal">
				<h3>客户端事件 / 网络异常日志</h3>
				<div className="loglines">
					{logs.length === 0 && <div className="muted">操作两个客户端后，这里显示幂等确认、重连续传与拒绝接管等事件。</div>}
					{logs.map((l) => (
						<div key={l.id} className={`logline ${l.kind}`}>
							<span className="ts">{new Date(l.at).toLocaleTimeString()}</span>
							{l.text}
						</div>
					))}
				</div>
			</section>

			<footer className="foot muted">
				权威结论只来自 Go 服务：预测位置为空心描边，确认位置为实心；发生分歧时客户端直接吸附到服务端坐标，不做平滑掩盖。
			</footer>
		</div>
	);
}
