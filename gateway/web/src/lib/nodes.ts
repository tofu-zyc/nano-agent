// 设置项挂在拓扑图的哪个节点上。网关给每个设置项一个 group（见 config.Nodes），这里是 group 到节点的对应。
// 面板只按这里的 group 渲染设置；不认识的 group 放到网关节点的「其他设置」里，保证一项都不丢。
import type { FieldState, Target } from './types';

type Home = { label: string; icon: string; color: string; target: Target };

export const HOMES: Record<string, Home> = {
  agent: { label: 'Agent 默认值', icon: 'persona', color: 'var(--c-agent)', target: { type: 'agentDefaults' } },
  gateway: { label: '网关', icon: 'hub', color: 'var(--c-gateway)', target: { type: 'gateway' } },
  web: { label: '网页', icon: 'globe', color: 'var(--c-web)', target: { type: 'channel', name: 'web' } },
  cli: { label: '终端', icon: 'terminal', color: 'var(--c-cli)', target: { type: 'channel', name: 'cli' } },
  telegram: { label: 'Telegram', icon: 'send', color: 'var(--c-telegram)', target: { type: 'channel', name: 'telegram' } },
  feishu: { label: '飞书', icon: 'feishu', color: 'var(--c-feishu)', target: { type: 'channel', name: 'feishu' } },
  model: { label: '模型', icon: 'sparkles', color: 'var(--c-model)', target: { type: 'model' } },
  memory: { label: '上下文与记忆', icon: 'brain', color: 'var(--c-memory)', target: { type: 'memory' } },
  search: { label: '网络搜索', icon: 'search', color: 'var(--c-search)', target: { type: 'search' } },
  skills: { label: '技能', icon: 'book', color: 'var(--c-skill)', target: { type: 'skills' } },
  local: { label: '本机', icon: 'hub', color: 'var(--c-kernel)', target: { type: 'machine', name: '本机' } },
  machines: { label: '接入机器', icon: 'plus', color: 'var(--c-kernel)', target: { type: 'machines' } },
};

/** 设置项所在的节点；不认识的组算在网关上（网关节点的「其他设置」） */
export function homeOf(f: Pick<FieldState, 'group'>): Home {
  return HOMES[f.group] ?? HOMES.gateway;
}

/** 某个节点面板要显示哪些组的设置 */
export function groupsOf(t: Target): string[] {
  switch (t.type) {
    case 'agentDefaults':
      return ['agent'];
    case 'gateway':
      return ['gateway'];
    case 'model':
      return ['model'];
    case 'memory':
      return ['memory'];
    case 'search':
      return ['search'];
    case 'skills':
      return ['skills'];
    case 'machines':
      return ['machines'];
    case 'machine':
      return t.name === '本机' ? ['local'] : [];
    case 'channel':
      return HOMES[t.name] ? [t.name] : [];
  }
  return [];
}

/** 网页不认识的组（网关比网页新时可能出现）：在网关节点的「其他设置」里显示 */
export function strayFields(fields: FieldState[]): FieldState[] {
  return fields.filter((f) => !HOMES[f.group]);
}
