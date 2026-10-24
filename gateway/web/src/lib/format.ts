// 各种显示用的小函数
import type { EndpointInfo, SessionInfo } from './types';

export function ago(ms: number): string {
  if (!ms) return '';
  const d = (Date.now() - ms) / 1000;
  if (d < 45) return '刚刚';
  if (d < 3600) return `${Math.round(d / 60)} 分钟前`;
  if (d < 86400) return `${Math.round(d / 3600)} 小时前`;
  if (d < 86400 * 2) return '昨天';
  const t = new Date(ms);
  return `${t.getMonth() + 1}-${t.getDate()}`;
}

export function clock(ms: number): string {
  const t = new Date(ms);
  return `${String(t.getHours()).padStart(2, '0')}:${String(t.getMinutes()).padStart(2, '0')}`;
}

export function num(n: number | undefined): string {
  if (!n) return '0';
  if (n >= 1e6) return (n / 1e6).toFixed(n >= 1e7 ? 0 : 1) + 'M';
  if (n >= 1e4) return (n / 1e3).toFixed(0) + 'k';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}

export function pct(part: number | undefined, whole: number | undefined): string {
  if (!whole) return '—';
  return Math.round(((part || 0) * 100) / whole) + '%';
}

export function since(ms: number): string {
  const s = Math.max(0, (Date.now() - ms) / 1000);
  if (s < 60) return `${Math.round(s)} 秒`;
  if (s < 3600) return `${Math.round(s / 60)} 分钟`;
  if (s < 86400) return `${(s / 3600).toFixed(1)} 小时`;
  return `${(s / 86400).toFixed(1)} 天`;
}

/** 太长的路径从中间省略 */
export function shortPath(p: string, max = 28): string {
  if (p.length <= max) return p;
  const win = /^[A-Za-z]:[\\/]/.test(p) || p.startsWith('\\\\');
  const sep = win ? '\\' : '/';
  const parts = p.split(win ? /[\\/]/ : '/');
  const last = parts.pop() || '';
  let head = parts.join(sep);
  const keep = max - last.length - 2;
  if (keep < 4) return '…' + p.slice(-max + 1);
  head = head.slice(0, keep);
  return head + '…' + sep + last;
}

export function truncate(s: string, n: number): string {
  const r = Array.from(s);
  return r.length > n ? r.slice(0, n - 1).join('') + '…' : s;
}

export function osLabel(os?: string): string {
  switch (os) {
    case 'windows':
      return 'Windows';
    case 'macos':
      return 'macOS';
    case 'linux':
      return 'Linux';
  }
  return '';
}

/** 机器的种类：网关所在的本机、装了内核的电脑、网关通过 SSH 连的服务器 */
export const machineMeta: Record<string, { label: string; icon: string }> = {
  local: { label: '本机', icon: 'hub' },
  kernel: { label: '电脑', icon: 'laptop' },
  ssh: { label: 'SSH 服务器', icon: 'server' },
};

export const channelMeta: Record<string, { label: string; color: string; icon: string }> = {
  cli: { label: '命令行', color: 'var(--c-cli)', icon: 'terminal' },
  web: { label: '网页', color: 'var(--c-web)', icon: 'globe' },
  telegram: { label: 'Telegram', color: 'var(--c-telegram)', icon: 'send' },
  feishu: { label: '飞书', color: 'var(--c-feishu)', icon: 'feishu' },
  life: { label: '内心', color: 'var(--accent-2)', icon: 'sparkles' }, // 拟人型的内心活动（只能看）
};

export const stateLabel: Record<string, string> = {
  online: '在线',
  starting: '连接中',
  error: '出错',
  stopped: '已停止',
  idle: '等待接入',
  disabled: '未启用',
};

export const modeMeta: Record<string, { label: string; desc: string }> = {
  auto: { label: '自动', desc: '工作目录内的读写和命令直接执行' },
  ask: { label: '审批', desc: '写文件、执行命令前要批准' },
  readonly: { label: '只读', desc: '只能读文件和搜索' },
};

export const toolIcon: Record<string, string> = {
  Read: 'file',
  Write: 'pen',
  Edit: 'edit',
  Grep: 'search',
  Shell: 'terminal',
};

export type NoteKind = 'memory' | 'recall' | 'life';
const noteBlocks: [NoteKind, string, string][] = [
  ['memory', '[记忆]', '[/记忆]'],
  ['recall', '[想起来的]', '[/想起来的]'],
  ['life', '[近况]', '[/近况]'],
];

/** 拆开网关加在用户消息前面的东西：[系统] 环境说明、记忆说明（[记忆]…[/记忆] 等多行块）、[时间 · 来源]、群聊里的 [昵称] */
export function parseUser(content: string): { env: string[]; notes: { kind: NoteKind; text: string }[]; time?: string; via?: string; sender?: string; text: string } {
  const env: string[] = [];
  const notes: { kind: NoteKind; text: string }[] = [];
  let rest = content;
  for (let guard = 0; guard < 20; guard++) {
    if (rest.startsWith('[系统] ')) {
      const i = rest.indexOf('\n');
      if (i < 0) {
        env.push(rest.slice(5));
        rest = '';
        break;
      }
      env.push(rest.slice(5, i));
      rest = rest.slice(i + 1);
      continue;
    }
    const nb = noteBlocks.find(([, open]) => rest.startsWith(open));
    if (!nb) break;
    const end = rest.indexOf(nb[2]);
    if (end < 0) break;
    notes.push({ kind: nb[0], text: rest.slice(nb[1].length, end).trim() });
    rest = rest.slice(end + nb[2].length).replace(/^\n/, '');
  }
  let time: string | undefined;
  let via: string | undefined;
  const t = rest.match(/^\[(\d{4}-\d{2}-\d{2}) (周.) (\d{2}:\d{2})(?: · ([^\]\n]{1,40}))?\] /);
  if (t) {
    time = `${t[1].slice(5)} ${t[2]} ${t[3]}`;
    via = t[4];
    rest = rest.slice(t[0].length);
  }
  let sender: string | undefined;
  const g = rest.match(/^\[([^\]\n]{1,40})\] /);
  if (g) {
    sender = g[1];
    rest = rest.slice(g[0].length);
  }
  return { env, notes, time, via, sender, text: rest };
}

/** 消息来源（时间戳里写的）对应的渠道 */
export const viaChannel: Record<string, string> = { 命令行: 'cli', 网页: 'web', Telegram: 'telegram', 飞书: 'feishu' };

/** 聊天窗口的名字：渠道，加上私聊对方的名字 */
export function endpointLabel(e: EndpointInfo): string {
  const label = channelMeta[e.channel]?.label ?? e.channel;
  return e.name && e.channel !== 'cli' ? `${label} · ${e.name}` : label;
}

/** 会话接着的渠道（去重）；没有窗口接着的会话只能在网页里聊 */
export function sessionChannels(s: SessionInfo): string[] {
  const out: string[] = [];
  for (const e of s.endpoints ?? []) if (!out.includes(e.channel)) out.push(e.channel);
  return out.length ? out : ['web'];
}

export function homeShort(p: string): string {
  return p.replace(/^\/home\/[^/]+/, '~').replace(/^\/root(?=\/|$)/, '~');
}

/** 一个时间点写短一点：今天 08:00、明天 08:00、10-24 08:00（浏览器所在时区） */
export function whenText(t?: number | string): string {
  if (!t) return '无';
  const d = new Date(t);
  if (isNaN(d.getTime()) || d.getFullYear() < 2000) return '无';
  const hm = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
  const day = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const diff = Math.round((day(d) - day(new Date())) / 86400000);
  if (diff === 0) return '今天 ' + hm;
  if (diff === 1) return '明天 ' + hm;
  if (diff === -1) return '昨天 ' + hm;
  return `${d.getMonth() + 1}-${d.getDate()} ${hm}`;
}
