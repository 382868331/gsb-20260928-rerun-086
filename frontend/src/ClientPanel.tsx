import { useEffect, useMemo, useRef, useState } from 'react';
import { GameClient, type ChaosConfig, type TransportStatus } from './api';
import { Board } from './Board';
import { applyAuthority, applyLocalInput, initialView, type ClientViewState } from './state';
import type { Command, EnqueueResult, QueuedInput, RoomInfo, SnapshotEvent } from './types';

interface Props {
	room: RoomInfo;
	sessionIndex: 0 | 1;
	chaos: ChaosConfig;
	duplicateSeen: (s: string) => void;
}

const CMDS: { cmd: Command; label: string }[] = [
	{ cmd: 'up', label: '↑' },
	{ cmd: 'left', label: '←' },
	{ cmd: 'wait', label: '等待' },
	{ cmd: 'right', label: '→' },
	{ cmd: 'down', label: '↓' },
];

export function ClientPanel({ room, sessionIndex, chaos, duplicateSeen }: Props) {
	const session = room.sessions[sessionIndex];
	const meta = room.chars.find((c) => c.id === session.charId)!;
	const clientRef = useRef<GameClient | null>(null);
	if (!clientRef.current) {
		clientRef.current = new GameClient(room.roomId, session.id, session.charId);
	}
	const client = clientRef.current;
	const cfg = useMemo(
		() => ({ width: room.config.width, height: room.config.height, obstacles: room.config.obstacles }),
		[room],
	);
	const [view, setView] = useState<ClientViewState>(() =>
		initialView(
			room.chars.map((c) => ({ id: c.id, x: c.x, y: c.y, facing: 'right', appliedSeq: 0 })),
			session.charId,
		),
	);
	const [status, setStatus] = useState<TransportStatus>('connecting');
	const seqRef = useRef(0);
	const [notice, setNotice] = useState<string>('');
	const [queued, setQueued] = useState<QueuedInput[]>([]);

	useEffect(() => {
		client.chaos = chaos;
	}, [chaos, client]);

	useEffect(() => {
		client.connectStream();
		const offEvt = client.onEvent((ev: SnapshotEvent) => {
			try {
				if (!ev || ev.type !== 'snapshot' || !Array.isArray(ev.snapshot?.chars)) return;
				const mine = ev.snapshot.chars.some((ch) => ch.id === client.charId);
				if (!mine) {
					duplicateSeen('收到不含本角色的异常快照，已忽略（页面保持可用）');
					return;
				}
				setView((prev) => {
					const appliedForMe = (ev.applied ?? []).filter((a) => a.charId === client.charId).map((a) => a.seq);
					return applyAuthority(prev, client.charId, ev.snapshot, appliedForMe, cfg);
				});
			} catch (err) {
				duplicateSeen(`快照处理失败已隔离：${String(err)}`);
			}
		});
		const offStatus = client.onStatus(setStatus);
		const offQueue = client.onQueue(setQueued);
		return () => {
			offEvt();
			offStatus();
			offQueue();
			client.closeStream();
		};
	}, [client, cfg]);

	async function issue(cmd: Command) {
		// Inputs may be issued at any time, including during a blackout:
		// the transport holds them and the pending layer keeps them visible.
		const seq = ++seqRef.current;
		setView((prev) => applyLocalInput(prev, client.charId, seq, cmd, cfg));
		try {
			const { status: code, result } = await client.sendInput(seq, cmd, (_code, dup: EnqueueResult) => {
				duplicateSeen(`重复送达 seq=${seq}：服务端幂等响应 ${dup.status}`);
			});
			if (code === 403) {
				duplicateSeen(`seq=${seq} 被拒绝（403，会话无权操作该角色）`);
			} else if (result?.status === 'stale') {
				duplicateSeen(`seq=${seq} 旧输入已处理，返回 stale ack（未执行第二遍）`);
			}
		} catch (err) {
			duplicateSeen(`seq=${seq} 发送失败：${String(err)}`);
		}
	}

	function blink() {
		const ms = chaos.blinkMs || 2500;
		setNotice(`模拟短暂断开 ${ms}ms …`);
		client.startBlink(ms);
		setTimeout(() => setNotice(''), ms + 50);
	}

	function sendStale() {
		// Manually redeliver the very first input again to prove idempotency.
		const seq = Math.max(1, seqRef.current);
		client.sendInput(seq, 'right', (_c, r) => duplicateSeen(`手动重发 seq=${seq}：${r.status}`));
	}

	function tryHijack() {
		// Attempt to drive this character using the OTHER client's session.
		const other = room.sessions[sessionIndex === 0 ? 1 : 0];
		fetch(`/api/rooms/${room.roomId}/inputs`, {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ sessionId: other.id, charId: session.charId, seq: 999000 + Date.now() % 100000, cmd: 'up' }),
		}).then(async (r) => {
			const j = await r.json().catch(() => ({}));
			duplicateSeen(`接管尝试：HTTP ${r.status}（${j.status ?? '拒绝'}），角色未被接管`);
		});
	}

	const me = view.auth.find((c) => c.id === client.charId);
	const predicting = view.pending.length > 0;
	const offline = status === 'disconnected' || status === 'reconnecting';
	return (
		<section className="panel">
			<header className="panel-head">
				<span className="dot" style={{ background: meta.color }} />
				<div>
					<h2>{meta.name} · 本地客户端 {sessionIndex + 1}</h2>
					<code className="mono small">session {session.id.slice(0, 16)}…</code>
				</div>
				<span className={`status status-${status}`}>{statusLabel(status)}</span>
			</header>

		<Board
			width={cfg.width}
			height={cfg.height}
			obstacles={cfg.obstacles}
			chars={room.chars}
			states={view.auth}
			localCharId={client.charId}
			predX={view.predX}
			predY={view.predY}
			predFacing={view.predFacing}
			hasPending={predicting}
			lastCorrectionAt={view.lastCorrection?.at ?? null}
		/>

		<div className="legend">
			<span className="tag confirmed">■ 服务端确认</span>
			<span className="tag predicted">◈ 本地预测（未确认）</span>
			{view.lastCorrection && <span className="tag corrected">↩ 偏差已按服务端修正</span>}
		</div>
		{view.pending.length > 0 && (
			<div className="pendingbox">
				{view.pending.map((p) => (
					<code key={p.seq} className="pending-item">
						seq {p.seq} · {p.cmd} · 预测 ({p.predX},{p.predY}) · {offline ? '断线留存' : '在途'}
					</code>
				))}
			</div>
		)}

		<div className="stats">
			<span>权威 tick: <b>{view.tick}</b></span>
			<span>已确认 seq: <b>{me?.appliedSeq ?? 0}</b></span>
			<span>待确认: <b className={predicting ? 'warn' : ''}>{view.pending.length}</b></span>
			{offline && <span className="warn">断线队列: <b>{queued.length}</b></span>}
		</div>
		{(queued.length > 0 || offline) && (
			<div className="queuebox">
				<span>
					{offline
						? `链路断开：${queued.length} 条未确认输入本地保留，重连后按原 seq 接续发送`
						: `重连完成，${queued.length} 条积压输入正在投递…`}
				</span>
				{queued.map((q) => (
					<code key={q.seq} className="queue-item">seq {q.seq} · {q.cmd}</code>
				))}
			</div>
		)}
		<div className="hash mono small">snapshot hash: {view.hash.slice(0, 20) || '…'}</div>

		<div className="dpad">
			{CMDS.map((c) => (
				<button key={c.cmd} onClick={() => issue(c.cmd)}>
					{c.label}
				</button>
			))}
		</div>
		<div className="chaos-actions">
			<button onClick={blink}>模拟短暂断开</button>
			<button onClick={sendStale}>重发旧输入</button>
			<button onClick={tryHijack}>尝试接管对方</button>
		</div>
		{notice && <div className="notice">{notice}</div>}
		</section>
	);
}

function statusLabel(s: TransportStatus): string {
	switch (s) {
		case 'online': return '已连接';
		case 'connecting': return '连接中';
		case 'reconnecting': return '重连中（Last-Event-ID 续传）';
		case 'disconnected': return '短暂断开';
	}
}
