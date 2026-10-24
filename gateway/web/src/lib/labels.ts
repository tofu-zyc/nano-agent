// 选项、工具、机器人种类的中文名字：面板里别把英文原样丢给用户。
export const enumLabels: Record<string, Record<string, string>> = {
  mode: { auto: '自动', ask: '每次问', readonly: '只读' },
  'agent.mode': { auto: '自动', ask: '每次问', readonly: '只读' },
  'workspace.mode': { auto: '自动', ask: '审批', readonly: '只读' },
  group_eagerness: { off: '不主动说', low: '少插话', normal: '正常', high: '多插话' },
  thinking: { none: '不思考', minimal: '最少', low: '少', medium: '中', high: '多', auto: '模型默认' },
  'access.guest': { none: '只能聊天', readonly: '能读文件' },
  'llm.provider': { openai: 'OpenAI 兼容', mock: '假模型' },
  'llm.modalities': { text: '文字', image: '图片' },
  'feishu.domain': { feishu: '飞书', lark: 'Lark' },
};

export function optionLabel(key: string, v: string): string {
  return enumLabels[key]?.[v] ?? v;
}

// 工具白名单里给人看的名字（括号里是模型看到的原名，鼠标停上去能看到）
export const toolLabels: Record<string, string> = {
  Read: '读文件',
  Write: '写文件',
  Edit: '改文件',
  Grep: '搜文件内容',
  Shell: '执行命令',
  WebSearch: '联网搜索',
  WebFetch: '打开网页',
  Skill: '用技能',
  Schedule: '定时任务',
  Gateway: '查网关状态',
  GatewayAdmin: '改网关设置',
  Message: '给群 / 别的 agent 发消息',
};

export const toolLabel = (n: string) => toolLabels[n] ?? n;

export const botKindLabel = (k: string) => (k === 'feishu' ? '飞书' : k === 'telegram' ? 'Telegram' : k);
