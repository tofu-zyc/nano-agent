// 和网关接口对应的类型

// 一张用户发来的图片（存在网关的数据目录里，历史里只留引用）
export type Attachment = { id: string; name?: string; mime: string; size: number; source?: string };

export type Usage = {
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens?: number;
  reasoning_tokens?: number;
};

export type ChannelStatus = { state: string; identity?: string; error?: string; since: number };
export type ChannelInfo = { name: string; enabled: boolean; status: ChannelStatus };

// 一个在线的内核（agent）。local 表示它和网关在同一台机器上，managed 表示是网关自己启动的
export type KernelInfo = { node: string; landlock: number; tools: string[]; since: number; local: boolean; managed?: boolean };

// 技能放在网关上
export type SkillInfo = { name: string; description: string; dir: string };

// 聊天命令（输入框里打 / 时提示）
export type CommandInfo = { name: string; args?: string; desc: string; admin?: boolean };

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

// 工作目录：机器 + 路径。会话可以同时挂好几个；default 表示新会话默认挂上
export type WorkspaceInfo = { id: string; host: string; path: string; default?: boolean; sessions: number; online: boolean };

// 模型接口（端点）：配置文件里的 [llm] 是 id 为 config 的那个，只能在设置里改；网页里可以再加别的
export type ModelEndpoint = {
  id: string;
  name: string;
  kind: 'openai' | 'mock';
  base_url: string;
  proxy?: string;
  has_key: boolean; // 有没有设置 API key（值不返回）
  config: boolean;
};

// 一个可以选的模型。ID 是 端点/模型名
export type ModelItem = {
  id: string;
  endpoint: string;
  name: string;
  label: string;
  context_window: number;
  max_tokens: number;
  max_history_tokens: number;
  thinking: string;
  thinking_levels: string[] | null;
  modalities?: string[] | null; // 能接收什么：text、image
  owned_by?: string;
  default: boolean;
  config: boolean;
};

export type ModelsView = { endpoints: ModelEndpoint[]; models: ModelItem[]; default: string };

// 端点 /models 拉回来的一个模型
export type RemoteModel = { id: string; owned_by?: string; context_window?: number; modalities?: string[] | null; added: boolean };

// 会话挂载的一个工作目录。how：local（跑这一轮的内核在同一台机器上，自己执行）、
// gateway（交给网关在那台机器上执行）、down（那台机器现在用不了）
export type MountInfo = { id: string; host: string; path: string; how: 'local' | 'gateway' | 'down' };

// 聊天窗口：终端、和机器人的某个私聊。每个窗口接在一个会话上
// who 是在这个窗口说话的人，admin 表示他是管理员
export type EndpointInfo = { key: string; channel: string; name: string; who?: string; admin: boolean; session: string };

export type SessionInfo = {
  sid: string;
  channel: string; // 主要的渠道（第一个接着的窗口所在的），配色用
  endpoints: EndpointInfo[] | null;
  title: string;
  workspaces: MountInfo[]; // 挂载的工作目录，空表示只聊天
  kernel: string; // 跑这一轮的内核（没在跑时是下一轮会用的）
  run_node: string; // 正在跑这一轮的内核
  running: boolean;
  pending: number;
  last_at: number;
  messages: number;
  mode: string;
  thinking: string;
  thinking_levels: string[] | null; // 这个模型的思考强度可选值
  model: string; // 这个会话选的模型 ID，空表示默认模型
  model_used: string; // 实际用的模型 ID
  model_label: string; // 实际用的模型的名字
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
  models: ModelsView;
  workspace: { mode: string };
  skills: { dir: string; list: SkillInfo[] };
  commands: CommandInfo[];
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
  images?: Attachment[]; // 用户消息附带的图片
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
  | { kind: 'user'; text: string; name?: string; via?: string; channel?: string; images?: Attachment[]; at: number }
  | { kind: 'reasoning'; text: string }
  | { kind: 'content'; text: string }
  | { kind: 'tool'; id: string; name: string; detail: string; args?: string; ok?: boolean; result?: string; resultDetail?: string }
  | { kind: 'say'; text: string; at: number };

// 拓扑图里可以点开的对象
export type Target =
  | { type: 'gateway' }
  | { type: 'model' }
  | { type: 'kernel'; node?: string } // 在线的内核（node 是某一个）、技能和连接设置
  | { type: 'machine'; name?: string } // 一台机器；没有名字时是所有机器
  | { type: 'workspace'; id?: string } // 一个工作目录；没有 ID 时是工作目录列表和默认设置
  | { type: 'channel'; name: string }
  | { type: 'session'; sid: string };

export type Persona = { name: string; sid?: string; size: number; mod: number; sessions: string[] };
