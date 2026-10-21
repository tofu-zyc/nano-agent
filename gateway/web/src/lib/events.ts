// 把事件写成人看得懂的一行，活动条和日志用
import type { Ev } from './types';
import { channelMeta, stateLabel, truncate } from './format';

export type Line = { icon: string; text: string; tone?: 'ok' | 'err' | 'warn' | 'accent'; sid?: string };

export function describe(e: Ev, title: (sid: string) => string): Line | null {
  const who = e.sid ? title(e.sid) : '';
  switch (e.t) {
    case 'message':
      return { icon: 'chat', text: `${who} · ${e.name ?? ''}：${truncate(e.text ?? '', 60)}`, sid: e.sid };
    case 'run':
      if (e.kind === 'start') return null;
      if (e.text === 'ok') {
        const u = e.data?.usage;
        const cache = u?.prompt_tokens ? `，缓存命中 ${Math.round(((u.cached_tokens || 0) * 100) / u.prompt_tokens)}%` : '';
        return { icon: 'check', text: `${who} 完成（${e.data?.calls ?? 0} 次模型调用${cache}，${((e.data?.ms ?? 0) / 1000).toFixed(1)}s）`, tone: 'ok', sid: e.sid };
      }
      return { icon: 'alert', text: `${who} 结束：${e.text}`, tone: e.text === 'cancelled' ? 'warn' : 'err', sid: e.sid };
    case 'tool':
      return { icon: 'terminal', text: `${who} · ${e.name}  ${truncate(e.text ?? '', 70)}`, sid: e.sid };
    case 'tool_result':
      return e.ok ? null : { icon: 'alert', text: `${who} · 工具失败：${truncate(e.text ?? '', 60)}`, tone: 'err', sid: e.sid };
    case 'approval':
      return { icon: 'shield', text: `${who} 需要审批 #${e.n}：${e.name}`, tone: 'warn', sid: e.sid };
    case 'kernel':
      return e.data ? { icon: 'shield', text: `内核 ${e.data.node} 已连接`, tone: 'ok' } : { icon: 'alert', text: `内核 ${e.name} 断开了`, tone: 'err' };
    case 'channel': {
      const st = e.data?.status?.state;
      const label = channelMeta[e.name ?? '']?.label ?? e.name;
      if (st === 'online') return { icon: 'link', text: `${label} 在线${e.data.status.identity ? '：' + e.data.status.identity : ''}`, tone: 'ok' };
      if (st === 'error') return { icon: 'alert', text: `${label} 出错：${truncate(e.data.status.error ?? '', 60)}`, tone: 'err' };
      return { icon: 'link', text: `${label} ${stateLabel[st] ?? st}` };
    }
    case 'config':
      return { icon: 'sliders', text: '配置已更新', tone: 'accent' };
    case 'persona':
      return { icon: 'persona', text: `人格 ${e.name} 已更新`, tone: 'accent' };
  }
  return null;
}
