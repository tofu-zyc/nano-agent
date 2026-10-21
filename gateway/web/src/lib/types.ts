// 和网关接口对应的类型

export type Usage = {
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens?: number;
  reasoning_tokens?: number;
};

export type ChannelStatus = { state: string; identity?: string; error?: string; since: number };
export type ChannelInfo = { name: string; enabled: boolean; status: ChannelStatus };

export type KernelInfo = { node: string; landlock: number; tools: string[]; since: number };

export type SessionInfo = {
  sid: string;
  channel: string;
  title: string;
  running: boolean;
  pending: number;
  last_at: number;
  messages: number;
  dirs: string[] | null;
  ro: string[] | null;
  mode: string;
  thinking: string;
  persona: string;
  persona_file: string;
  usage: Usage;
  compacted: number;
};

export type Approval = { n: number; sid: string; id: string; name: string; detail: string; from: string; at: number };

export type Totals = {
  runs: number;
  model_calls: number;
  prompt_tokens: number;
  cached_tokens: number;
  completion_tokens: number;
  reasoning_tokens: number;
};

export type Overview = {
  gateway: { started: number; kernel_listen: string; web_listen: string; config_dir: string; data_dir: string };
  kernel: KernelInfo | null;
  model: {
    provider: string;
    model: string;
    base_url: string;
    thinking: string;
    thinking_style: string;
    levels: string[] | null;
    context_window: number;
    max_history: number;
  };
  channels: ChannelInfo[];
  sessions: SessionInfo[] | null;
  approvals: Approval[];
  totals: Totals;
  workspace: { dirs: string[] | null; mode: string };
  access: { admins: string[] | null; guest: string };
};

export type FieldState = {
  key: string;
  group: string;
  section?: string;
  label: string;
  help?: string;
  type: 'string' | 'int' | 'float' | 'bool' | 'enum' | 'list' | 'secret' | 'json';
  options?: string[];
  restart?: boolean;
  readonly?: boolean;
  advanced?: boolean;
  keywords?: string;
  value: any;
  source: 'file' | 'web' | 'env' | 'default';
  is_set?: boolean;
  pending?: boolean;
};

export type ToolCall = { id: string; type: string; function: { name: string; arguments: string } };

export type Msg = {
  role: 'user' | 'assistant' | 'tool' | 'system';
  content?: string;
  tool_calls?: ToolCall[];
  tool_call_id?: string;
  reasoning?: string;
  usage?: Usage;
  ts?: number;
};

export type Ev = {
  t: string;
  at: number;
  sid?: string;
  kind?: string;
  id?: string;
  name?: string;
  text?: string;
  ok?: boolean;
  n?: number;
  data?: any;
};

// 一轮处理过程中实时收到的内容（结束后换成从历史里读的完整记录）
export type LiveItem =
  | { kind: 'user'; text: string; name?: string; at: number }
  | { kind: 'reasoning'; text: string }
  | { kind: 'content'; text: string }
  | { kind: 'tool'; id: string; name: string; detail: string; args?: string; ok?: boolean; result?: string; resultDetail?: string }
  | { kind: 'say'; text: string; at: number };

// 拓扑图里可以点开的对象
export type Target =
  | { type: 'gateway' }
  | { type: 'model' }
  | { type: 'kernel' }
  | { type: 'workspace' }
  | { type: 'channel'; name: string }
  | { type: 'session'; sid: string }
  | { type: 'dir'; path: string };

export type Persona = { name: string; sid?: string; size: number; mod: number; sessions: string[] };
