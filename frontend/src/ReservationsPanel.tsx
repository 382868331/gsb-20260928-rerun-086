import { useEffect, useRef, useState } from 'react';
import { fetchRoomSnapshot, reservationOp, submitReservation } from './api';
import type { ResvConflict, RoomInfo, Snapshot } from './types';

interface Props {
	room: RoomInfo;
	log: (text: string, kind?: 'info' | 'warn' | 'ok') => void;
}

const PHASE_LABEL: Record<string, string> = {
	pending: '等待裁决',
	approved: '已批准(未开始)',
	active: '进行中',
	completed: '已完成',
	released: '已提前归还',
	cancelled: '已撤回',
	expired: '已过期(未满足)',
};

function conflictText(c: ResvConflict, regionName: (id: string) => string): string {
	const where = c.kind === 'capacity' ? `区域 ${regionName(c.region ?? '')} 容量不足` : `角色 ${c.charId} 时间重叠`;
	const withWhom = c.with ? `，与 …${c.with.slice(-6)} 冲突` : '';
	return `${where} [${c.from}, ${c.to})${withWhom}`;
}

// ReservationsPanel is the group multi-region pass booking view: it shows
// each request's overall phase, the actual occupancy of every region, and
// the concrete conflict intervals of requests still waiting.
export function ReservationsPanel({ room, log }: Props) {
	const regions = room.config.regions ?? [];
	const [snap, setSnap] = useState<Snapshot | null>(null);
	const [selChars, setSelChars] = useState<string[]>(room.chars.map((c) => c.id));
	const [selRegions, setSelRegions] = useState<string[]>(regions.map((r) => r.id));
	const [startIn, setStartIn] = useState(20);
	const [duration, setDuration] = useState(40);
	const [priority, setPriority] = useState(5);
	const lastReq = useRef<{ requestId: string; charIds: string[]; regions: string[]; start: number; end: number; priority: number } | null>(null);

	useEffect(() => {
		let stop = false;
		const pull = () => fetchRoomSnapshot(room.roomId).then((s) => !stop && setSnap(s)).catch(() => undefined);
		void pull();
		const id = setInterval(pull, 1000);
		return () => {
			stop = true;
			clearInterval(id);
		};
	}, [room.roomId]);

	const regionName = (id: string) => regions.find((r) => r.id === id)?.name ?? id;
	const charName = (id: string) => room.chars.find((c) => c.id === id)?.name ?? id;
	const tick = snap?.tick ?? 0;
	const reservations = snap?.reservations ?? [];

	function toggle(list: string[], v: string, set: (x: string[]) => void) {
		set(list.includes(v) ? list.filter((x) => x !== v) : [...list, v]);
	}

	async function submit(reuse?: boolean) {
		const body = reuse && lastReq.current
			? lastReq.current
			: {
				requestId: `rsv_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 8)}`,
				charIds: selChars,
				regions: selRegions,
				start: tick + startIn,
				end: tick + startIn + duration,
				priority,
			};
		if (!body.charIds.length || !body.regions.length) {
			log('组队预约需要至少一名角色和一个区域', 'warn');
			return;
		}
		try {
			const { status, result } = await submitReservation(room.roomId, body);
			lastReq.current = body;
			const rv = result.reservation;
			if (status === 200) {
				log(`重复提交 ${body.requestId.slice(-6)}：服务端幂等返回原申请（${PHASE_LABEL[rv.phase] ?? rv.phase}），未重复占用名额`, 'warn');
			} else {
			log(`组队预约 ${body.requestId.slice(-6)} 已登记：${rv.charIds.map(charName).join('+')} @ ${rv.regions.map(regionName).join('+')} [${rv.start},${rv.end}) 优先级${rv.priority} → ${PHASE_LABEL[rv.phase] ?? rv.phase}`, rv.phase === 'approved' ? 'ok' : 'info');
			}
			setSnap(await fetchRoomSnapshot(room.roomId));
		} catch (e) {
			log(`预约提交失败：${String(e)}`, 'warn');
		}
	}

	async function doOp(id: string, op: 'cancel' | 'release') {
		try {
			const r = await reservationOp(room.roomId, id, op);
			const label = op === 'cancel' ? '撤回' : '提前归还';
			log(`${label} …${id.slice(-6)} → ${PHASE_LABEL[r.reservation.phase] ?? r.reservation.phase}`, 'ok');
			setSnap(await fetchRoomSnapshot(room.roomId));
		} catch (e) {
			log(`${op} 失败：${String(e)}`, 'warn');
		}
	}

	// Current occupancy per region at the current tick.
	function occupancy(regionId: string): { used: number; cap: number } {
		const cap = regions.find((r) => r.id === regionId)?.capacity ?? 0;
		let used = 0;
		for (const rv of reservations) {
			if (rv.phase === 'active' && rv.regions.includes(regionId)) used += rv.charIds.length;
		}
		return { used, cap };
	}

	return (
		<section className="resv">
			<h3>多区域组队通行预约 <span className="muted small">当前 tick {tick}</span></h3>

			<div className="resv-form">
				<div className="control-group">
					<label>组队角色</label>
					{room.chars.map((c) => (
						<label key={c.id} className="inline">
							<input
								type="checkbox"
								checked={selChars.includes(c.id)}
								onChange={() => toggle(selChars, c.id, setSelChars)}
							/>
							{c.name}
						</label>
					))}
				</div>
				<div className="control-group">
					<label>区域（可多个，整体批准）</label>
					{regions.map((r) => (
						<label key={r.id} className="inline">
							<input
								type="checkbox"
								checked={selRegions.includes(r.id)}
								onChange={() => toggle(selRegions, r.id, setSelRegions)}
							/>
							{r.name}(容量{r.capacity})
						</label>
					))}
				</div>
				<div className="control-group">
					<label>开始于 +{startIn} tick</label>
					<input type="range" min={5} max={100} step={5} value={startIn} onChange={(e) => setStartIn(Number(e.target.value))} />
					<label>时长 {duration} tick</label>
					<input type="range" min={10} max={120} step={10} value={duration} onChange={(e) => setDuration(Number(e.target.value))} />
					<label>优先级 {priority}</label>
					<input type="range" min={0} max={10} value={priority} onChange={(e) => setPriority(Number(e.target.value))} />
				</div>
				<div className="control-group">
					<button onClick={() => void submit()}>提交组队预约</button>
					<button disabled={!lastReq.current} onClick={() => void submit(true)} title="用同一 requestId 重复提交，验证幂等">
						重复提交上次申请
					</button>
				</div>
			</div>

			<div className="resv-regions">
				{regions.map((r) => {
					const o = occupancy(r.id);
					return (
						<div key={r.id} className="resv-region">
							<b>{r.name}</b>
							<span className="mono"> 当前占用 {o.used}/{o.cap}</span>
							<div className="resv-bar">
								{Array.from({ length: o.cap }, (_, i) => (
									<span key={i} className={i < o.used ? 'slot used' : 'slot'} />
								))}
							</div>
						</div>
					);
				})}
			</div>

			<table className="resv-table">
				<thead>
					<tr><th>申请</th><th>角色</th><th>区域</th><th>窗口</th><th>优先级</th><th>阶段</th><th>等待原因 / 冲突区间</th><th>操作</th></tr>
				</thead>
				<tbody>
					{reservations.length === 0 && (
						<tr><td colSpan={8} className="muted">尚无预约。提交一个跨区域的组队申请试试；让两个客户端申请重叠时段可观察竞争与晋升。</td></tr>
					)}
					{reservations.map((rv) => (
						<tr key={rv.id} className={`phase-${rv.phase}`}>
							<td className="mono">…{rv.id.slice(-6)}</td>
							<td>{rv.charIds.map(charName).join(' + ')}</td>
							<td>{rv.regions.map(regionName).join(' + ')}</td>
							<td className="mono">[{rv.start}, {rv.end})</td>
							<td>{rv.priority}{rv.boost > 0 ? ` (+${rv.boost} 等待晋升)` : ''}</td>
							<td><span className={`badge phase-${rv.phase}`}>{PHASE_LABEL[rv.phase] ?? rv.phase}</span></td>
							<td className="small">
								{rv.phase === 'pending' && (rv.conflicts?.length ? '' : '等待空位…')}
								{(rv.conflicts ?? []).map((c, i) => (
									<div key={i} className="conflict">{conflictText(c, regionName)}</div>
								))}
							</td>
							<td>
								{(rv.phase === 'pending' || rv.phase === 'approved') && (
									<button onClick={() => void doOp(rv.id, 'cancel')}>撤回</button>
								)}
								{rv.phase === 'active' && (
									<button onClick={() => void doOp(rv.id, 'release')}>提前归还</button>
								)}
							</td>
						</tr>
					))}
				</tbody>
			</table>
		</section>
	);
}
