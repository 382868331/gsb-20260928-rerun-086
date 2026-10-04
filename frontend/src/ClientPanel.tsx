import { useEffect, useMemo, useRef, useState } from 'react';
import { cancelRound, GameClient, type ChaosConfig, type TransportStatus } from './api';
import { Board } from './Board';
import { applyAuthority, applyLocalInput, initialView, loadView, reasonLabel, saveView, type ClientViewState } from './state';
import type { Command, EnqueueResult, RoomInfo, RoundView, SnapshotEvent } from './types';

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
		const initialResources = room.resources.map((r) => ({ ...r, remaining: r.total, holders: [] }));
		return initialView(
			room.chars.map((c) => ({ id: c.id, x: c.x, y: c.y, facing: 'right', appliedSeq: 0 })),
			room.rules,
			initialResources,
		);
	});
	const [restoredFromDisk] = useState(() => loadView(room.roomId, session.charId) !== null);
	const [status, setStatus] = useState<TransportStatus>('connecting');
	const seqRef = useRef(loadView(room.roomId, session.charId)?.nextSeq ?? 0);
	const [notice, setNotice] = useState<string>('');
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
				// rule basis; the authority decides which version to apply.
				void client
					.sendInput(p.seq, p.cmd, p.ruleVersion, undefined, { resource: p.resource, dependsOn: p.dependsOn })
					.then(({ status: code, result }) => {
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
				return applyAuthority(prev, client.charId, ev.snapshot, ev.applied ?? [], cfg, ev.rules, ev.rounds);
			});
		});
		const offStatus = client.onStatus(setStatus);
		return () => {
			offEvt();
			offStatus();
		client.closeStream();
		};
	}, [client, cfg]);

	async function issue(cmd: Command, resource?: string) {
		const seq = ++seqRef.current;
		const basis = rulesVersionRef.current;
		const dependsOn = depNext && seq > 1 ? seq - 1 : 0;
		setView((prev) => {
			const next = applyLocalInput(prev, client.charId, seq, cmd, cfg, { resource, dependsOn });
			saveView(room.roomId, client.charId, next, seqRef.current);
			return next;
		});
		try {
			const { status: code, result } = await client.sendInput(
				seq,
				cmd,
				basis,
				(_code, dup: EnqueueResult) => {
					duplicateSeen(`重复送达 seq=${seq}：服务端幂等响应 ${dup.status}`);
				},
				{ resource, dependsOn },
			);
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

	function sendStale() {
		// Manually redeliver the very first input again to prove idempotency.
		const seq = Math.max(1, seqRef.current);
		client.sendInput(seq, 'right', rulesVersionRef.current, (_c, r) => duplicateSeen(`手动重发 seq=${seq}：${r.status}`));
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
	const charName = (id: string) => room.chars.find((c) => c.id === id)?.name ?? id;
	const resourceName = (id: string) => room.resources.find((r) => r.id === id)?.name ?? id;
	const openRoundOf = (seq: number) =>
		view.rounds.find((r) => r.status === 'open' && r.claims.some((c) => c.charId === client.charId && c.seq === seq));
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
			<span>待确认: <b className={predicting ? 'warn' : ''}>{view.pending.length}</b></span>
		</div>
		<div className="hash mono small">snapshot hash: {view.hash.slice(0, 20) || '…'}</div>

		{view.pending.length > 0 && (
			<ul className="pending-list">
				{view.pending.map((p) => (
					<li key={p.seq} className={p.ruleVersion < view.rules.version ? 'stale-basis' : ''}>
						seq {p.seq} {p.cmd} → 预测 ({p.predX},{p.predY}) · 依据规则 v{p.ruleVersion}
						{p.ruleVersion < view.rules.version && `（当前 v${view.rules.version}，待权威判定）`}
						{p.cmd === 'claim' && openRoundOf(p.seq) && ` · 轮 ${openRoundOf(p.seq)!.id} 裁决中`}
					</li>
				))}
			</ul>
		)}
		{restoredFromDisk && (
			<div className="notice">已从本地恢复会话与待确认队列（刷新不新建房间，继续核对原预测）。</div>
		)}
		{view.rounds.length > 0 && (
			<div className="rounds">
				<div className="rounds-title">共享裁决明细（两端一致）：</div>
				{view.rounds.map((r) => (
					<RoundLine
						key={r.id}
						round={r}
						charName={charName}
						resourceName={resourceName}
						onCancel={() => {
							cancelRound(room.roomId, r.id)
								.then(() => duplicateSeen(`已显式取消争用轮 ${r.id}，收集的预测将以 round-cancelled 拒绝`))
								.catch((e) => duplicateSeen(`取消轮 ${r.id} 失败：${String(e)}`));
						}}
					/>
				))}
			</div>
		)}

		<div className="dpad">
				{CMDS.map((c) => (
					<button key={c.cmd} onClick={() => issue(c.cmd)}>
					{c.label}
				</button>
			))}
		</div>
		<div className="chaos-actions">
			{room.resources.map((r) => {
				const live = view.resources.find((v) => v.id === r.id);
				const remaining = live ? live.remaining : r.total;
				return (
					<button key={r.id} onClick={() => issue('claim', r.id)}>
						领取{r.name}（剩 {remaining}/{r.total}）
					</button>
				);
			})}
			<label className="inline">
				<input type="checkbox" checked={depNext} onChange={(e) => setDepNext(e.target.checked)} />
				下一动作依赖上一动作
			</label>
		</div>
		{view.resources.length > 0 && (
			<div className="resources">
				{view.resources.map((r) => {
					const myPendingClaim = view.pending.some((p) => p.cmd === 'claim' && p.resource === r.id);
					return (
						<div key={r.id} className="resource-line">
							<b>{r.name}</b>：剩余 {r.remaining}/{r.total}
							{r.holders.length > 0 && (
								<span>
									，持有者{' '}
									{r.holders
										.map((h) => {
											const meta = room.chars.find((c) => c.id === h.charId);
											return `${meta?.name ?? h.charId}（seq ${h.seq}）`;
										})
										.join('、')}
								</span>
							)}
							{myPendingClaim && <span className="warn">，本地预测领取待确认</span>}
						</div>
					);
				})}
			</div>
		)}
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
							<button className="reconfirm" onClick={() => issue(p.cmd, p.resource)}>
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

function RoundLine({
	round,
	charName,
	resourceName,
	onCancel,
}: {
	round: RoundView;
	charName: (id: string) => string;
	resourceName: (id: string) => string;
	onCancel: () => void;
}) {
	const claims = round.claims.map((c) => `${charName(c.charId)} seq ${c.seq}（依据 v${c.ruleVersion}）`).join('、');
	if (round.status === 'open') {
		return (
			<div className="round-line open">
				轮 <b>{round.id}</b>（{resourceName(round.resource)}）裁决中：已收集 {claims || '无'}
				{round.waitingFor.length > 0 && (
					<span className="warn">，等待 {round.waitingFor.map(charName).join('、')} 的输入</span>
				)}
				<span className="muted"> · 截止 {new Date(round.deadlineMs).toLocaleTimeString()}</span>
				<button className="reconfirm" onClick={onCancel}>取消本轮</button>
			</div>
		);
	}
	if (round.status === 'adjudicated') {
		return (
			<div className="round-line adjudicated">
				轮 <b>{round.id}</b>（{resourceName(round.resource)}）已裁决（{round.closeReason === 'complete' ? '全员到齐' : round.closeReason}）：
				胜者 <b>{round.winner ? charName(round.winner) : '无'}</b>；参与 {claims || '无'}
			</div>
		);
	}
	return (
		<div className="round-line cancelled">
			轮 <b>{round.id}</b>（{resourceName(round.resource)}）已取消（
			{round.closeReason === 'deadline' ? '收集超时' : '手动取消'}）
			{round.missing && round.missing.length > 0 && `，缺少 ${round.missing.map(charName).join('、')} 的输入`}
			：未授予任何人；参与 {claims || '无'}
		</div>
	);
}
