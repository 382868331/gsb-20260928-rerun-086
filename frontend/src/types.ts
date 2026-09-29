export interface CharStateDTO {
  id: string;
  x: number;
  y: number;
  vx: number;
  vy: number;
  lastSeq: number;
}

export interface FrameDTO {
  tick: number;
  hash: string;
  chars: Record<string, CharStateDTO>;
}

export interface EventDTO {
  id: number;
  kind: string;
  atTick: number;
  data: never;
}
