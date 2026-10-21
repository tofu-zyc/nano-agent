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
  const parts = p.split('/');
  const last = parts.pop() || '';
  let head = parts.join('/');
  const keep = max - last.length - 2;
  if (keep < 4) return '…' + p.slice(-max + 1);
  head = head.slice(0, keep);
  return head + '…/' + last;
}

export function truncate(s: string, n: number): string {
  const r = Array.from(s);
  return r.length > n ? r.slice(0, n - 1).join('') + '…' : s;
}

export const channelMeta: Record<string, { label: string; color: string; icon: string }> = {
  cli: { label: '命令行', color: 'var(--c-cli)', icon: 'terminal' },
  web: { label: '网页', color: 'var(--c-web)', icon: 'globe' },
  telegram: { label: 'Telegram', color: 'var(--c-telegram)', icon: 'send' },
  feishu: { label: '飞书', color: 'var(--c-feishu)', icon: 'feishu' },
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

/** 拆开网关加在用户消息前面的东西：[系统] 环境说明、[时间 · 来源]、群聊里的 [昵称] */
export function parseUser(content: string): { env: string[]; time?: string; via?: string; sender?: string; text: string } {
  const env: string[] = [];
  let rest = content;
  while (rest.startsWith('[系统] ')) {
    const i = rest.indexOf('\n');
    if (i < 0) {
      env.push(rest.slice(5));
      rest = '';
      break;
    }
    env.push(rest.slice(5, i));
    rest = rest.slice(i + 1);
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
  return { env, time, via, sender, text: rest };
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
