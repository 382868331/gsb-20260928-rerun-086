import { useEffect, useMemo, useRef, useState } from 'react';
import { reservationOp, submitReservation } from './api';
import type { ReservationView, RoomInfo, Snapshot, SnapshotEvent } from './types';

// ReservationPanel is the multi-region group reservation board. It keeps
// its own read-only SSE subscription so the whole-room view (every
// request's phase, per-region occupancy, wait reasons) is independent of
// either client panel's local prediction state.
export function ReservationPanel({ room, onLog }: { room: RoomInfo; onLog: (text: string, kind?: 'info' | 'warn' | 'ok') => void }) {
	const [snap, setSnap] = useState<Snapshot | null>(null);
	const [chars, setChars] = useState<string[]>(room.sessions.map((s) => s.charId));
	const [regions, setRegions] = useState<string[]>(() => (room.config.regions ?? []).slice(0, 2).map((r) => r.id));
	const [startIn, setStartIn] = useState(15);
	const [duration, setDuration] = useState(30);
	const [priority, setPriority] = useState(1);
	const counter = useRef(0);
	const lastRequest = useRef<{
		requestId: string;
		chars: string[];
		regions: string[];
		start: number;
		end: number;
		priority: number;
	} | null>(null);

	useEffect(() => {
		const es = new EventSource(`/api/rooms/${room.roomId}/events`);
		es.onmessage = (e) => {
			try {
				const ev = JSON.parse(e.data) as SnapshotEvent;
				if (ev.type === 'snapshot') setSnap(ev.snapshot);
			} catch {
				// ignore heartbeats
			}
		};
		return () => es.close();
	}, [room.roomId]);

	const tick = snap?.tick ?? 0;
	const reservations = useMemo(() => snap?.reservations ?? [], [snap]);
	const regionViews = useMemo(() => snap?.regions ?? [], [snap]);
	const charName = (id: string) => room.chars.find((c) => c.id === id)?.name ?? id.slice(0, 10);
	const regionName = (id: string) => room.config.regions?.find((r) => r.id === id)?.name ?? id;

	function toggle(list: string[], v: string, set: (x: string[]) => void) {
		set(list.includes(v) ? list.filter((x) => x !== v) : [...list, v]);
	}

	async function submit(reuse?: boolean) {
		const sessionId = room.sessions[0]?.id;
		if (!sessionId) return;
		let body;
		if (reuse && lastRequest.current) {
			body = { sessionId, ...lastRequest.current };
		} else {
			counter.current += 1;
			const req = {
				requestId: `req-${counter.current}-${Math.random().toString(36).slice(2, 8)}`,
				chars,
				regions,
				start: tick + startIn,
				end: tick + startIn + duration,
				priority,
			};
			lastRequest.current = req;
			body = { sessionId, ...req };
		}
		if (body.chars.length === 0 || body.regions.length === 0) {
			onLog('组队申请至少需要一名角色和一个区域', 'warn');
			return;
		}
		const { status, result } = await submitReservation(room.roomId, body);
		if (status === 200 || result?.status === 'duplicate') {
			onLog(`申请 ${body.requestId} 重复提交：服务端幂等返回原身份，不重复占用名额`, 'warn');
		} else if (status === 202) {
			onLog(`已提交组队申请 ${body.requestId}：${body.chars.map(charName).join('+')} @ ${body.regions.map(regionName).join('+')} [${body.start},${body.end}) 优先级 ${body.priority}`, 'ok');
		} else {
			onLog(`申请 ${body.requestId} 被拒绝（HTTP ${status}）`, 'warn');
		}
	}

	async function op(id: string, action: 'cancel' | 'return') {
		const sessionId = room.sessions[0]?.id;
		if (!sessionId) return;
		const code = await reservationOp(room.roomId, id, action, sessionId);
		onLog(
			action === 'cancel'
				? `撤回申请 ${id}（HTTP ${code}）：仅开始前生效，迟到撤回是显式空操作`
				: `提前归还 ${id}（HTTP ${code}）：释放剩余时段，重复归还是幂等空操作`,
		);
	}

	return (
		<section className="resv">
			<h3>多区域组队通行预约</h3>
			<div className="resv-layout">
				<div className="resv-form">
					<div className="resv-row">
						<span>角色</span>
						{room.chars.map((c) => (
							<label key={c.id} className="inline">
								<input
									type="checkbox"
									checked={chars.includes(c.id)}
									onChange={() => toggle(chars, c.id, setChars)}
								/>
								{c.name}
							</label>
						))}
					</div>
					<div className="resv-row">
						<span>区域</span>
						{(room.config.regions ?? []).map((r) => (
							<label key={r.id} className="inline">
								<input
									type="checkbox"
									checked={regions.includes(r.id)}
									onChange={() => toggle(regions, r.id, setRegions)}
								/>
								{r.name}（容量 {r.capacity}）
							</label>
						))}
					</div>
					<div className="resv-row">
						<label className="inline">
							开始后
							<input type="number" min={1} value={startIn} onChange={(e) => setStartIn(Number(e.target.value))} />
							tick
						</label>
						<label className="inline">
							时长
							<input type="number" min={1} value={duration} onChange={(e) => setDuration(Number(e.target.value))} />
							tick
						</label>
						<label className="inline">
							优先级
							<input type="number" min={0} max={10} value={priority} onChange={(e) => setPriority(Number(e.target.value))} />
						</label>
					</div>
					<div className="resv-row">
						<button onClick={() => void submit()}>提交组队申请</button>
						<button disabled={!lastRequest.current} onClick={() => void submit(true)}>
							重复发送上一申请（幂等）
						</button>
						<span className="muted small">当前 tick {tick}</span>
					</div>
					<div className="resv-regions">
						{regionViews.map((r) => (
							<div key={r.id} className="resv-region">
								<span className="resv-region-name">{r.name}</span>
								<span className="resv-bar">
									{Array.from({ length: r.capacity }, (_, i) => (
										<i key={i} className={i < r.used ? 'slot used' : 'slot'} />
									))}
								</span>
								<span className="muted small">
									{r.used}/{r.capacity}
									{r.occupants.length > 0 ? ` 占用：${r.occupants.join(', ')}` : ''}
								</span>
							</div>
						))}
					</div>
				</div>
				<div className="resv-list">
					{reservations.length === 0 && <div className="muted">暂无申请。提交后这里展示整体阶段、各区域占用与等待原因。</div>}
					{reservations.map((v) => (
						<ReservationCard
							key={v.id}
							v={v}
							tick={tick}
							charName={charName}
							regionName={regionName}
							onCancel={() => void op(v.id, 'cancel')}
							onReturn={() => void op(v.id, 'return')}
						/>
					))}
				</div>
			</div>
		</section>
	);
}

const PHASE_LABEL: Record<string, string> = {
	pending: '等待裁决',
	approved: '已批准（待开始）',
	active: '进行中',
	completed: '已完成',
	released: '已提前归还',
	cancelled: '已撤回',
	rejected: '已拒绝',
};

function ReservationCard({
	v,
	tick,
	charName,
	regionName,
	onCancel,
	onReturn,
}: {
	v: ReservationView;
	tick: number;
	charName: (id: string) => string;
	regionName: (id: string) => string;
	onCancel: () => void;
	onReturn: () => void;
}) {
	const effPriority = v.priority + Math.min(v.windowsWaited ?? 0, 5);
	return (
		<div className={`resv-card state-${v.state}`}>
			<div className="resv-card-head">
				<b className="mono">{v.id}</b>
				<span className={`phase phase-${v.state}`}>{PHASE_LABEL[v.state] ?? v.state}</span>
				{(v.state === 'pending' || v.state === 'approved') && <button onClick={onCancel}>撤回</button>}
				{v.state === 'active' && <button onClick={onReturn}>提前归还</button>}
			</div>
			<div className="small">
				{v.chars.map(charName).join(' + ')} @ {v.regions.map(regionName).join(' + ')} 时段 [{v.start},{v.end}) 优先级 {v.priority}
				{v.state === 'pending' && (v.windowsWaited ?? 0) > 0 && (
					<span className="muted">（已等待 {v.windowsWaited} 个裁决边界，晋升后有效优先级 {effPriority}）</span>
				)}
				{v.returnedTick ? <span className="muted">（tick {v.returnedTick} 提前归还剩余时段）</span> : null}
				{v.reason ? <span className="warn-text">（{v.reason === 'expired' ? '等待超时，窗口已过' : v.reason}）</span> : null}
			</div>
			{v.state === 'pending' && tick < v.start && <div className="muted small">等待原因：容量/角色时间冲突，见下方冲突区间；当前 tick {tick}</div>}
			{(v.conflicts ?? []).length > 0 && (
				<ul className="resv-conflicts">
					{(v.conflicts ?? []).map((cf, i) => (
						<li key={i}>
							{cf.kind === 'region' ? `区域「${regionName(cf.id)}」` : `角色「${charName(cf.id)}」`}
							在 [{cf.start},{cf.end}) 被 <span className="mono">{cf.by}</span> 占用
						</li>
					))}
				</ul>
			)}
		</div>
	);
}

