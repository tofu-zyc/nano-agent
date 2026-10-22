// 和网关接口对应的类型

export type Usage = {
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens?: number;
  reasoning_tokens?: number;
};

export type ChannelStatus = { state: string; identity?: string; error?: string; since: number };
export type ChannelInfo = { name: string; enabled: boolean; status: ChannelStatus };

// 一个在线的内核（agent）。local 表示它和网关在同一台机器上
export type SkillInfo = { name: string; description: string };
export type KernelInfo = { node: string; landlock: number; tools: string[]; skills: SkillInfo[]; since: number; local: boolean };

// 一台能放工作目录的机器：网关所在的本机、装了内核的电脑、网关通过 SSH 连的服务器
export type MachineInfo = {
  name: string;
  kind: 'local' | 'kernel' | 'ssh';
  online: boolean; // 上面的工作目录现在能用
  landlock: number;
  state?: string;
  detail?: string;
  kernels: string[]; // 这台机器上在线的内核
};

// 工作目录：机器 + 路径。会话从列表里选一个；default 是新会话的默认
export type WorkspaceInfo = { id: string; host: string; path: string; default?: boolean; sessions: number; online: boolean };

// 聊天窗口：终端、和机器人的某个私聊。每个窗口接在一个会话上
// who 是在这个窗口说话的人，admin 表示他是管理员
export type EndpointInfo = { key: string; channel: string; name: string; who?: string; admin: boolean; session: string };

export type SessionInfo = {
  sid: string;
  channel: string; // 主要的渠道（第一个接着的窗口所在的），配色用
  endpoints: EndpointInfo[] | null;
  title: string;
  workspace: string; // 工作目录的 ID，空表示只聊天
  host: string; // 工作目录所在的机器
  path: string;
  host_online: boolean;
  run_node: string; // 正在跑这一轮的内核
  run_gateway: boolean; // 这一轮的工具交给网关执行（工作目录不在内核那台机器上）
  running: boolean;
  pending: number;
  last_at: number;
  messages: number;
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
  kernels: KernelInfo[];
  model: {
    provider: string;
    model: string;
    base_url: string;
    thinking: string;
    levels: string[] | null;
    context_window: number;
    max_history: number;
  };
  channels: ChannelInfo[];
  sessions: SessionInfo[] | null;
  approvals: Approval[];
  totals: Totals;
  machines: MachineInfo[];
  workspaces: WorkspaceInfo[];
  workspace: { mode: string };
  access: { admins: string[] | null; guest: string };
};

export type FieldState = {
  key: string;
  group: string;
  section?: string;
  label: string;
  help?: string;
  type: 'string' | 'int' | 'float' | 'bool' | 'enum' | 'list' | 'secret' | 'json' | 'proxy';
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
  | { kind: 'user'; text: string; name?: string; via?: string; channel?: string; at: number }
  | { kind: 'reasoning'; text: string }
  | { kind: 'content'; text: string }
  | { kind: 'tool'; id: string; name: string; detail: string; args?: string; ok?: boolean; result?: string; resultDetail?: string }
  | { kind: 'say'; text: string; at: number };

// 拓扑图里可以点开的对象
export type Target =
  | { type: 'gateway' }
  | { type: 'model' }
  | { type: 'kernel' } // 内核的连接设置和在线的内核
  | { type: 'machine'; name?: string } // 一台机器；没有名字时是所有机器
  | { type: 'workspace'; id?: string } // 一个工作目录；没有 ID 时是工作目录列表和默认设置
  | { type: 'channel'; name: string }
  | { type: 'session'; sid: string };

export type Persona = { name: string; sid?: string; size: number; mod: number; sessions: string[] };
