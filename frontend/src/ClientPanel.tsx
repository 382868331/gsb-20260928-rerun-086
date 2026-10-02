import { useEffect, useMemo, useRef, useState } from 'react';
import { GameClient, type ChaosConfig, type TransportStatus } from './api';
import { Board } from './Board';
import { applyAuthority, applyLocalInput, initialView, loadView, saveView, type ClientViewState } from './state';
import type { Command, EnqueueResult, RoomInfo, SnapshotEvent } from './types';

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
	const [view, setView] = useState<ClientViewState>(() => {
		const restored = loadView(room.roomId, session.charId);
		if (restored && restored.view.auth.some((c) => c.id === session.charId)) return restored.view;
		return initialView(room.chars.map((c) => ({ id: c.id, x: c.x, y: c.y, facing: 'right', energy: 10, appliedSeq: 0 })));
	});
	const [status, setStatus] = useState<TransportStatus>('connecting');
	const seqRef = useRef(loadView(room.roomId, session.charId)?.nextSeq ?? 0);
	const [notice, setNotice] = useState<string>('');
	const currentCfg = useMemo(
		() => ({
			...cfg,
			blocked: view.rules?.blocked ?? [],
			costs: view.rules?.costs,
		}),
		[cfg, view.rules],
	);
	useEffect(() => {
		saveView(room.roomId, client.charId, view, seqRef.current);
	}, [room.roomId, client.charId, view]);

	useEffect(() => {
		client.chaos = chaos;
	}, [chaos, client]);

	useEffect(() => {
		client.connectStream();
		const restored = loadView(room.roomId, client.charId);
		if (restored) {
			client.lastTick = restored.view.tick;
			restored.view.pending.forEach((p) => {
			void client.sendInput(p.seq, p.cmd, p.ruleVersion ?? 1).then(({ status: code, result }) => {
					if (code === 403) duplicateSeen(`seq=${p.seq} 恢复重发被会话校验拒绝`);
					else if (result?.status === 'stale') duplicateSeen(`seq=${p.seq} 恢复重发命中 stale，未重复执行`);
				});
			});
		}
		const offEvt = client.onEvent((ev: SnapshotEvent) => {
			setView((prev) => {
				return applyAuthority(prev, client.charId, ev.snapshot, ev.applied ?? [], {
					...cfg,
					blocked: ev.snapshot.rules?.blocked ?? [],
					costs: ev.snapshot.rules?.costs,
				});
			});
		});
		const offStatus = client.onStatus(setStatus);
		return () => {
			offEvt();
			offStatus();
		client.closeStream();
		};
	}, [client, cfg]);

	async function issue(cmd: Command) {
		const seq = ++seqRef.current;
		const basisVersion = view.ruleVersion || 1;
		setView((prev) => {
			const next = applyLocalInput(prev, client.charId, seq, cmd, currentCfg);
			saveView(room.roomId, client.charId, next, seqRef.current);
			return next;
		});
		try {
			const { status: code, result } = await client.sendInput(seq, cmd, basisVersion, (_code, dup: EnqueueResult) => {
				duplicateSeen(`重复送达 seq=${seq}：服务端幂等响应 ${dup.status}`);
			});
			if (code === 403) {
				duplicateSeen(`seq=${seq} 被拒绝（403，会话无权操作该角色）`);
			} else if (result?.status === 'stale') {
				duplicateSeen(`seq=${seq} 旧输入已处理，返回 stale ack（未执行第二遍）`);
			}
		} catch (err) {
			duplicateSeen(`seq=${seq} 发送失败，已保留本地待确认队列：${String(err)}`);
		}
	}

	async function confirmPending(seq: number, cmd: Command, ruleVersion: number) {
		const { status, result } = await client.confirmInput(seq, cmd, ruleVersion);
		if (status !== 202 || result?.status !== 'accepted') {
			duplicateSeen(`seq=${seq} 迟到确认未形成接受：${result?.status ?? 'failed'}`);
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
			obstacles={[...cfg.obstacles, ...(view.rules?.blocked ?? [])]}
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
			<span>规则: <b>v{view.ruleVersion}</b></span>
			<span>能量: <b>{me?.energy ?? view.predEnergy}</b></span>
			<span>权威 tick: <b>{view.tick}</b></span>
			<span>已确认 seq: <b>{me?.appliedSeq ?? 0}</b></span>
			<span>待确认: <b className={predicting ? 'warn' : ''}>{view.pending.length}</b></span>
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
		</div>
		{notice && <div className="notice">{notice}</div>}
		{view.rejectedInputs.length > 0 && (
			<div className="notice">
				最近权威拒绝/阻挡：{view.rejectedInputs.slice(-3).map((p) => `seq ${p.seq} ${p.msg ?? ''}`).join('；')}
			</div>
		)}
		{view.pending.some((p) => p.statusDetail === 'needs_confirmation') && (
			<div className="notice warn-box">
				{view.pending
					.filter((p) => p.statusDetail === 'needs_confirmation')
					.map((p) => (
						<div key={p.seq}>
							seq {p.seq} 原按规则 v{p.ruleVersion} 预测 ({p.originalPredX},{p.originalPredY})，当前规则 v{view.ruleVersion} 要求确认；
							<button onClick={() => void confirmPending(p.seq, p.cmd, p.ruleVersion ?? 1)}>按当前规则确认</button>
						</div>
					))}
			</div>
		)}
	</section>
);
}

function statusLabel(s: TransportStatus): string {
	switch (s) {
		case 'online': return '已连接';
		case 'connecting': return '连接中';
		case 'reconnecting': return '重连中（按权威 tick 续传）';
		case 'disconnected': return '短暂断开';
	}
}
