// 剧场（设计 §14）：场景记录的整理、字幕排法和放映用的时间。
// 字幕的排法要和后端 stage_export.go 的 renderSRT 一致（导出的字幕和放映同一个时间）。

export type StageRec = {
  seq?: number;
  kind: 'line' | 'cursors' | 'run' | 'model' | 'prefix' | 'op' | 'fork';
  ts: number;
  line?: number;
  from?: string;
  name?: string;
  say?: 'say' | 'narrate' | 'cue' | 'system';
  channel?: string;
  to?: string;
  text?: string;
  cursors?: Record<string, number>;
  run?: string;
  member?: string;
  status?: string;
  inner?: string;
  reasoning?: string;
  usage?: { prompt_tokens?: number; completion_tokens?: number; reasoning_tokens?: number; cached_tokens?: number };
  said?: number;
  model?: { endpoint: string; model: string; params: Record<string, unknown>; out: string; reason: string; err?: string };
  hash?: string;
  len?: number;
  op?: string;
  detail?: any;
};

export type DocItem = {
  kind: 'say' | 'narrate' | 'cue' | 'system' | 'inner';
  who?: string; // 说话的人；inner 是想这句话的成员
  to?: string; // cue 的对象
  channel?: string; // 私下频道的名字，公开为空
  text: string;
  line: number;
};

export type ShowOpts = { inner: boolean; channels: boolean };

/** 把场景记录排成正文：心里话放在它那句话前面；想了没说的一轮排在它发生的位置。 */
export function buildItems(recs: StageRec[], channelName: (id: string) => string, name: (id: string) => string): DocItem[] {
  const innerOf = new Map<number, StageRec>();
  for (const r of recs) if (r.kind === 'run' && (r.said ?? 0) > 0 && r.inner) innerOf.set(r.said!, r);
  const items: DocItem[] = [];
  let cur = 0;
  for (const r of recs) {
    if (r.kind === 'line') {
      cur = r.line ?? 0;
      const ch = r.channel ? channelName(r.channel) : '';
      const inner = innerOf.get(cur);
      if (inner) items.push({ kind: 'inner', who: name(inner.member ?? ''), channel: ch, text: inner.inner ?? '', line: cur });
      const it: DocItem = { kind: (r.say ?? 'say') as DocItem['kind'], channel: ch, text: r.text ?? '', line: cur };
      if (r.say === 'say') it.who = r.name;
      else if (r.say === 'cue') it.to = name(r.to ?? '');
      else if (r.say === 'system') it.text = (r.text ?? '').replace(/^\[系统\] /, '');
      items.push(it);
    } else if (r.kind === 'run' && !(r.said ?? 0) && r.inner) {
      items.push({ kind: 'inner', who: name(r.member ?? ''), text: r.inner, line: cur });
    }
  }
  return items;
}

/** 能分出的句子：记了 cursors 的。 */
export function forkable(recs: StageRec[]): Set<number> {
  const s = new Set<number>();
  for (const r of recs) if (r.kind === 'cursors' && r.line) s.add(r.line);
  return s;
}

export function splitSentences(s: string): string[] {
  const out: string[] = [];
  let cur = '';
  for (const ch of s) {
    cur += ch;
    if ('。！？!?'.includes(ch)) {
      if (cur.trim()) out.push(cur.trim());
      cur = '';
    }
  }
  if (cur.trim()) out.push(cur.trim());
  return out;
}

export const SRT = { splitOver: 40, runesPerS: 5, minMS: 1500, maxMS: 8000, gapMS: 300 };

export type Cue = { start: number; end: number; text: string; kind: DocItem['kind']; line: number; who?: string };

const runeLen = (s: string) => [...s].length;

/** 字幕：人和 agent 的公开发言、旁白各占一条；频道里的话和心里话按开关；cue 和系统提示不进字幕。 */
export function buildCues(items: DocItem[], o: ShowOpts): Cue[] {
  const cues: Cue[] = [];
  let t = 0;
  for (const it of items) {
    if (it.kind === 'cue' || it.kind === 'system' || (it.kind === 'inner' && !o.inner)) continue;
    if (it.channel && !o.channels) continue;
    const text = it.text.split(/\s+/).filter(Boolean).join(' ');
    let label = '';
    if (it.kind === 'inner') label = `（${it.who}心里）`;
    else if (it.kind === 'say') label = `${it.who}${it.channel ? '（私下）' : ''}：`;
    else if (it.kind === 'narrate' && it.channel) label = '（私下）';
    const pieces = runeLen(text) > SRT.splitOver ? splitSentences(text) : [text];
    pieces.forEach((p, i) => {
      const cue = (i === 0 ? label : '') + p;
      const ms = Math.min(Math.max(Math.floor((runeLen(cue) * 1000) / SRT.runesPerS), SRT.minMS), SRT.maxMS);
      cues.push({ start: t, end: t + ms, text: cue, kind: it.kind, line: it.line, who: it.who });
      t += ms + SRT.gapMS;
    });
  }
  return cues;
}
