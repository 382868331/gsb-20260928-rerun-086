import { EventDTO, FrameDTO } from "../types";

const j = async (r: Response) => {
  if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
  return r.json();
};

export async function createRoom(): Promise<{
  roomId: string;
  tokens: Record<string, string>;
  chars: string[];
  tickIntervalMs: number;
}> {
  return j(await fetch("/api/rooms", { method: "POST" }));
}

export async function joinRoom(roomId: string, token: string): Promise<{
  charId: string;
  chars: string[];
  tickIntervalMs: number;
  snapshot: FrameDTO;
}> {
  return j(
    await fetch(`/api/rooms/${roomId}/join`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ token }),
    }),
  );
}

export async function getReplay(roomId: string): Promise<{
  events: EventDTO[];
  tickHashes: string[];
  mismatchTicks: number[];
  replayedFrames: number;
  liveHash: string;
  loggedHash: string;
  currentHash: string;
}> {
  return j(await fetch(`/api/rooms/${roomId}/events?replay=1`));
}
