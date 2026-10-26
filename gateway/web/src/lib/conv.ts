// 把会话历史整理成对话视图要显示的块
import type { Attachment, Msg, Usage } from './types';
import { parseUser, type NoteKind } from './format';

export type ToolView = { id: string; name: string; detail: string; args: string; ok?: boolean; result?: string; media?: Attachment[] };

export type Block =
  | { type: 'env'; text: string }
  | { type: 'note'; kind: NoteKind; text: string }
  | { type: 'wake'; text: string } // agent 自己的会话里，网关放的整段说明（醒来时看到的情况、昨晚整理了什么）
  | { type: 'user'; text: string; time?: string; via?: string; sender?: string; images?: Attachment[] }
  | { type: 'assistant'; text: string; reasoning?: string; usage?: Usage; tools: ToolView[]; ts?: number; sender?: string; agent?: string };

export function toolDetail(name: string, args: string): string {
  let a: Record<string, any>;
  try {
    a = JSON.parse(args);
  } catch {
    return args.slice(0, 120);
  }
  switch (name) {
    case 'Shell':
      return String(a.command ?? '');
    case 'Read':
      return String(a.path ?? '') + (a.offset ? `（从第 ${a.offset} 行）` : '');
    case 'SendFile':
      return (Array.isArray(a.files) ? a.files : []).join('、');
    case 'Write':
      return `${a.path ?? ''}（${new TextEncoder().encode(String(a.content ?? '')).length} 字节）`;
    case 'Edit':
      return String(a.path ?? '');
    case 'Grep':
      return [a.pattern, a.glob, a.path].filter(Boolean).join('  ');
  }
  return args.slice(0, 120);
}

export function prettyArgs(args: string): string {
  try {
    return JSON.stringify(JSON.parse(args), null, 2);
  } catch {
    return args;
  }
}

export function buildBlocks(history: Msg[], inner = false, room = false): Block[] {
  const out: Block[] = [];
  const byId = new Map<string, ToolView>();
  for (const m of history) {
    if (m.role === 'user' && inner) {
      // 内心活动、整理记忆：没有人在说话，“用户消息”都是网关放的说明，整段显示
      const p = parseUser(m.content ?? '');
      for (const n of p.notes) out.push({ type: 'note', kind: n.kind, text: n.text });
      const text = [...p.env, p.text].filter((x) => x.trim()).join('\n');
      if (text) out.push({ type: 'wake', text });
    } else if (m.role === 'user') {
      const p = parseUser(m.content ?? '');
      for (const e of p.env) out.push({ type: 'env', text: e });
      for (const n of p.notes) out.push({ type: 'note', kind: n.kind, text: n.text });
      // 只有网关放的说明（群里的记忆说明、约好的提醒）：不画一个空的气泡
      if (p.text.trim() || m.images?.length) out.push({ type: 'user', text: p.text, time: p.time, via: p.via, sender: p.sender, images: m.images });
    } else if (m.role === 'assistant') {
      const tools: ToolView[] = (m.tool_calls ?? []).map((c) => {
        const t: ToolView = { id: c.id, name: c.function.name, args: c.function.arguments, detail: toolDetail(c.function.name, c.function.arguments) };
        byId.set(c.id, t);
        return t;
      });
      const speaker = room ? parseUser(m.content ?? '') : undefined;
      out.push({ type: 'assistant', text: speaker?.text ?? m.content ?? '', sender: speaker?.sender, agent: m.agent,
        reasoning: m.reasoning, usage: m.usage, tools, ts: m.ts });
    } else if (m.role === 'tool') {
      const t = byId.get(m.tool_call_id ?? '');
      if (t) {
        let c = m.content ?? '';
        t.ok = !c.startsWith('[执行失败] ');
        if (!t.ok) c = c.slice('[执行失败] '.length);
        t.result = c;
        // 同一个附件可能既在 images（读到/发给模型的）又在 sent（发出去的）里，
        // 按 id 去重；否则 Svelte 的 keyed each 会抛 each_key_duplicate，整段历史都画不出来。
        const media = [...new Map([...(m.images ?? []), ...(m.sent ?? [])].map((a) => [a.id, a])).values()];
        if (media.length) t.media = media;
      }
    }
  }
  return out;
}
