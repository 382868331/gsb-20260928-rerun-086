import { useEffect, useMemo, useRef, useState } from 'react';
import { GameClient, type ChaosConfig, type TransportStatus } from './api';
import { Board } from './Board';
import { applyAuthority, applyLocalInput, initialView, restoreView, type ClientViewState } from './state';
import { clearClient, loadClient, saveClient } from './storage';
import type { Command, EnqueueResult, RoomInfo, SavedClientState, SnapshotEvent } from './types';

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
	const restoredRef = useRef<SavedClientState | null | undefined>(undefined);
	if (restoredRef.current === undefined) {
		restoredRef.current = loadClient(room.roomId, session.id);
		if (!restoredRef.current || restoredRef.current.charId !== session.charId) {
			restoredRef.current = null;
		}
	}
	const restored = restoredRef.current;
	const [view, setView] = useState<ClientViewState>(() =>
		restored
			? restoreView(restored)
			: initialView(room.chars.map((c) => ({ id: c.id, x: c.x, y: c.y, facing: 'right', appliedSeq: 0 }))),
	);
	const [status, setStatus] = useState<TransportStatus>(restored ? 'connecting' : 'connecting');
	const seqRef = useRef(restored?.nextSeq ?? 0);
	const [notice, setNotice] = useState<string>(
		restored ? `已从本地保存恢复：tick ${restored.view.tick}、待确认 ${restored.pending.length}、已确认 seq ${restored.confirmedSeq}` : '',
	);
	const [restoredFrom, setRestoredFrom] = useState<number | null>(restored?.savedAt ?? null);
	const viewRef = useRef(view);
	viewRef.current = view;

	useEffect(() => {
		client.chaos = chaos;
	}, [chaos, client]);

	useEffect(() => {
		client.connectStream();
		const offEvt = client.onEvent((ev: SnapshotEvent) => {
			setView((prev) => {
				// Idle heartbeats carry empty lists; refused inputs are folded
				// as final authority decisions for prediction review.
				const refusedForMe = (ev.rejected ?? []).filter((r) => r.charId === client.charId);
				return applyAuthority(prev, client.charId, ev.snapshot, refusedForMe, cfg);
			});
		});
		const offStatus = client.onStatus(setStatus);
		return () => {
			offEvt();
			offStatus();
		client.closeStream();
		};
	}, [client, cfg]);

	// Persist after every state change: saving mid-session and reloading later
	// restores queue, ack position and predicted layer.
	useEffect(() => {
		saveClient({
			roomId: room.roomId, sessionId: session.id, charId: client.charId,
			nextSeq: seqRef.current, view,
		});
	}, [view, room.roomId, session.id, client.charId]);

	function issue(cmd: Command) {
		const seq = ++seqRef.current;
		setView((prev) => applyLocalInput(prev, client.charId, seq, cmd, cfg));
		// The outbox delivers in local seq order; offline inputs queue and
		// flush on reconnect. Duplicate redeliveries report the stale ack.
		client.queueInput(seq, cmd, (code, result, isDuplicate) => {
			if (isDuplicate) {
				duplicateSeen(`重复送达 seq=${seq}：服务端幂等响应 ${result?.status ?? '?'}`);
				return;
			}
			if (code === 403) {
				duplicateSeen(`seq=${seq} 被拒绝（403，会话无权操作该角色）`);
			} else if (result?.status === 'stale') {
				duplicateSeen(`seq=${seq} 旧输入已处理，返回 stale ack（未执行第二遍）`);
			}
		});
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
		fetch(`/api/rooms/${room.roomId}/inputs`, {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ sessionId: session.id, charId: session.charId, seq, cmd: 'right' }),
		}).then(async (r) => {
			const j = (await r.json().catch(() => ({}))) as EnqueueResult;
			duplicateSeen(`手动重发 seq=${seq}：${j.status ?? r.status}`);
		});
	}

	function clearSaved() {
		clearClient(room.roomId, session.id);
		setNotice('本地保存已清除（刷新后将以当前权威状态新建客户端视图）');
		setRestoredFrom(null);
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
	const rejectedItems = view.reviewed.filter((p) => p.status === 'rejected');
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

		<div className="stats">
			<span>权威 tick: <b>{view.tick}</b></span>
			<span>已确认 seq: <b>{me?.appliedSeq ?? 0}</b></span>
			<span>待确认: <b className={predicting ? 'warn' : ''}>{view.pending.length}</b></span>
			<span>本地队列: <b className={client.outboxLength ? 'warn' : ''}>{client.outboxLength}</b></span>
		</div>
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
			<button onClick={() => setNotice(`已保存到本地：tick ${view.tick}、待确认 ${view.pending.length}、下一 seq ${seqRef.current}`)}>
				保存当前状态
			</button>
			<button onClick={clearSaved}>清除本地保存</button>
		</div>
		{restoredFrom && (
			<div className="notice ok-note">恢复自 {new Date(restoredFrom).toLocaleTimeString()} 的本地保存，正在与权威状态继续核对</div>
		)}
		{rejectedItems.length > 0 && (
			<div className="review">
				<h4>离线输入回看（预测 → 最终确认）</h4>
				{rejectedItems.slice(-5).map((p) => (
					<div key={p.seq} className="review-item">
						<code>seq {p.seq} · {p.cmd}</code>
						<span>预测 ({p.predX},{p.predY})</span>
						<b className="bad">权威拒绝：{p.msg ?? p.reason}</b>
					</div>
				))}
			</div>
		)}
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
