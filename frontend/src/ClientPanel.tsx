import { useEffect, useMemo, useRef, useState } from 'react';
import { GameClient, type ChaosConfig, type TransportStatus } from './api';
import { Board } from './Board';
import { applyAuthority, applyLocalInput, initialView, loadView, reasonLabel, saveView, type ClientViewState } from './state';
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
		return initialView(
			room.chars.map((c) => ({ id: c.id, x: c.x, y: c.y, facing: 'right', appliedSeq: 0 })),
			room.rules,
			room.config.passes ?? 0,
		);
	});
	const [status, setStatus] = useState<TransportStatus>('connecting');
	const seqRef = useRef(loadView(room.roomId, session.charId)?.nextSeq ?? 0);
	const [notice, setNotice] = useState<string>('');
	// When checked, the next issued action declares a dependency on the
	// success of the previously issued one.
	const [depNext, setDepNext] = useState(false);
	const rulesVersionRef = useRef(view.rules.version);
	useEffect(() => {
		rulesVersionRef.current = view.rules.version;
	}, [view.rules.version]);
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
				// Late confirmations for saved inputs: resend with the ORIGINAL
				// rule basis and dependency; the authority decides the outcome.
				void client.sendInput(p.seq, p.cmd, p.ruleVersion, p.dependsOn ?? 0).then(({ status: code, result }) => {
					if (code === 403) duplicateSeen(`seq=${p.seq} 恢复重发被会话校验拒绝`);
					else if (result?.status === 'stale') duplicateSeen(`seq=${p.seq} 恢复重发命中 stale，未重复执行`);
				});
			});
		}
		const offEvt = client.onEvent((ev: SnapshotEvent) => {
			if (ev.rules && ev.rules.version !== rulesVersionRef.current) {
				duplicateSeen(
					`规则已切换 v${rulesVersionRef.current} → v${ev.rules.version}（封闭 ${ev.rules.sealed?.length ?? 0} 格）；待确认输入仍保留原规则依据`,
				);
			}
			setView((prev) => {
				return applyAuthority(prev, client.charId, ev.snapshot, ev.applied ?? [], cfg, ev.rules);
			});
		});
		const offStatus = client.onStatus(setStatus);
		return () => {
			offEvt();
			offStatus();
		client.closeStream();
		};
	}, [client, cfg]);

	async function issue(cmd: Command, dependsOn = 0) {
		const seq = ++seqRef.current;
		const basis = rulesVersionRef.current;
		setView((prev) => {
			const next = applyLocalInput(prev, client.charId, seq, cmd, cfg, dependsOn);
			saveView(room.roomId, client.charId, next, seqRef.current);
			return next;
		});
		try {
			const { status: code, result } = await client.sendInput(seq, cmd, basis, dependsOn, (_code, dup: EnqueueResult) => {
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

	function blink() {
		const ms = chaos.blinkMs || 2500;
		setNotice(`模拟短暂断开 ${ms}ms …`);
		client.startBlink(ms);
		setTimeout(() => setNotice(''), ms + 50);
	}

	// issueMaybeDep issues an action; when the dependency toggle is on it
	// depends on the success of the previously issued action.
	function issueMaybeDep(cmd: Command) {
		const dep = depNext && seqRef.current > 0 ? seqRef.current : 0;
		void issue(cmd, dep);
	}

	const charName = (id: string) => room.chars.find((c) => c.id === id)?.name ?? id;

	function sendStale() {
		// Manually redeliver the very first input again to prove idempotency.
		const seq = Math.max(1, seqRef.current);
		client.sendInput(seq, 'right', rulesVersionRef.current, 0, (_c, r) => duplicateSeen(`手动重发 seq=${seq}：${r.status}`));
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
			obstacles={cfg.obstacles}
			sealed={view.rules.sealed ?? []}
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
			<span>规则版本: <b>v{view.rules.version}</b></span>
			<span>能量: <b>{me?.energy ?? 0}</b></span>
			<span>剩余名额: <b className={view.passesLeft === 0 ? 'warn' : ''}>{view.passesLeft}</b></span>
			<span>持有名额: <b>{me?.passes ?? 0}</b></span>
			<span>待确认: <b className={predicting ? 'warn' : ''}>{view.pending.length}</b></span>
		</div>
		<div className="hash mono small">snapshot hash: {view.hash.slice(0, 20) || '…'}</div>

		{view.pending.length > 0 && (
			<ul className="pending-list">
				{view.pending.map((p) => (
					<li key={p.seq} className={p.ruleVersion < view.rules.version ? 'stale-basis' : ''}>
						seq {p.seq} {p.cmd} → {p.cmd === 'claim' ? '预测取得名额（待权威裁决）' : `预测 (${p.predX},${p.predY})`}
						{p.dependsOn ? ` · 依赖 seq ${p.dependsOn}` : ''} · 依据规则 v{p.ruleVersion}
						{p.ruleVersion < view.rules.version && `（当前 v${view.rules.version}，待权威判定）`}
					</li>
				))}
			</ul>
		)}

		{view.claims.length > 0 && (
			<div className="claims-list">
				<div className="claims-title">共享名额裁决（权威结论，双客户端一致）</div>
				{view.claims.slice(-5).map((v) => (
					<div key={`${v.charId}-${v.seq}`} className={`claim-item ${v.granted ? 'granted' : 'denied'}`}>
						tick {v.tick} · {charName(v.charId)} seq {v.seq}：
						{v.granted ? '✓ 取得通行名额' : `✗ ${reasonLabel(v.reason ?? 'resource-exhausted')}`}
						{v.ruleVersion ? `（依据 v${v.ruleVersion}，按 v${v.appliedRuleVersion ?? v.ruleVersion} 裁决）` : ''}
					</div>
				))}
			</div>
		)}

		<div className="dpad">
				{CMDS.map((c) => (
					<button key={c.cmd} onClick={() => issueMaybeDep(c.cmd)}>
					{c.label}
				</button>
			))}
		</div>
		<div className="claim-row">
			<button className="claim-btn" onClick={() => issueMaybeDep('claim')} disabled={view.rules.claimsClosed}>
				★ 领取通行名额{view.rules.claimsClosed ? '（规则已关闭）' : ''}
			</button>
			<label className="inline dep-toggle" title="勾选后，下一个动作依赖上一动作成功；若上一动作被权威拒绝，本动作获得独立的拒绝原因">
				<input type="checkbox" checked={depNext} onChange={(e) => setDepNext(e.target.checked)} />
				依赖上一动作
			</label>
		</div>
		<div className="chaos-actions">
			<button onClick={blink}>模拟短暂断开</button>
			<button onClick={sendStale}>重发旧输入</button>
			<button onClick={tryHijack}>尝试接管对方</button>
		</div>
		{notice && <div className="notice">{notice}</div>}
		{view.rejectedInputs.length > 0 && (
			<div className="notice rejected-list">
				<div>权威拒绝/阻挡记录（按原顺序保留，不会被修正抹掉）：</div>
				{view.rejectedInputs.slice(-4).map((p) => (
					<div key={`${p.seq}-${p.finalX}-${p.finalY}`} className="rejected-item">
						<span>seq {p.seq} {p.cmd}：{p.msg ?? reasonLabel(p.reason)}</span>
						{p.needsReconfirm && (
							<button className="reconfirm" onClick={() => issue(p.cmd)}>
								按 v{view.rules.version} 重新确认
							</button>
						)}
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
