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
export type KernelInfo = { node: string; os?: string; arch?: string; version?: string; landlock: number; tools: string[]; since: number; local: boolean; managed?: boolean };

// 技能放在网关上
export type SkillInfo = { name: string; description: string; dir: string };

// 聊天命令（输入框里打 / 时提示）
export type CommandInfo = { name: string; args?: string; desc: string; admin?: boolean };

// 一台能放工作目录的机器：网关所在的本机、装了内核的电脑、网关通过 SSH 连的服务器
export type MachineInfo = {
  name: string;
  kind: 'local' | 'kernel' | 'ssh';
  os?: string;
  online: boolean; // 上面的工作目录现在能用
  landlock: number;
  state?: string;
  detail?: string;
  kernels: string[]; // 这台机器上在线的内核
};

// 工作目录：机器 + 路径。会话可以同时挂好几个；default 表示新会话默认挂上
export type WorkspaceInfo = { id: string; host: string; path: string; os?: string; default?: boolean; sessions: number; online: boolean };

// 模型接口（端点）：模型服务的地址和密钥，一个接口上可以放多个模型
export type ModelEndpoint = {
  id: string;
  name: string;
  kind: 'openai' | 'mock';
  base_url: string;
  proxy?: string;
  has_key: boolean; // 有没有设置 API key（值不返回）
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
  agent: string; // 这个会话属于哪个 agent
  group?: boolean; // 群聊里的会话
  context: { auto_at: number; idle_for: string; total: number; window: number; persona: number; tools: number; summary: number; dialogue: number; tool_results: number; messages: number; last: Usage; compacted: number; compacted_at: number; model: string };
  sid: string;
  channel: string; // 主要的渠道（第一个接着的窗口所在的），配色用
  endpoints: EndpointInfo[] | null;
  title: string;
  workspaces: MountInfo[]; // 挂载的工作目录，空表示只读
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

// 版本检查的结果
export type UpdateInfo = {
  current: string;
  latest?: string;
  available: boolean;
  updatable: boolean;
  why?: string;
  checked?: number;
  error?: string;
  updating?: string;
};

export type RuntimeStatus = {
  running: boolean;
  pid: number;
  version: string;
  started: number;
  uptime_ms: number;
  mode: string;
  web_url?: string;
  kernel_listen: string;
  pending_restart: number;
  recent_errors?: string[];
  update?: UpdateInfo;
};

export type Overview = {
  gateway: { started: number; kernel_listen: string; web_listen: string; config_dir: string; data_dir: string; env_proxy?: string };
  runtime?: RuntimeStatus;
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
  search?: SearchInfo;
  schedules?: ScheduleSummary;
  memory?: MemoryInfo;
  agents?: AgentInfo[]; // 所有 agent（v0.5 起）
  bots?: BotInfo[]; // 所有机器人
  rooms?: RoomInfo[]; // 网关内部的群聊
};

/** 网络搜索的状态；key 是 settings（设置里填的）、endpoint:<接口名>（借用模型接口的）或空（没有可用的 key） */
export type SearchInfo = { enabled: boolean; provider: string; key: string; today: { date: string; search: number; fetch: number } };
export type ScheduleSummary = { total: number; enabled: number; running: number; next?: number; error?: string };
export type MemoryInfo = { summarized: number; archives: number };

export type FieldState = {
  key: string;
  group: string;
  section?: string;
  label: string;
  help?: string;
  type: 'string' | 'int' | 'float' | 'bool' | 'enum' | 'multi' | 'list' | 'secret' | 'json' | 'proxy';
  options?: string[];
  fixed?: string[]; // multi 里总是选上、不能去掉的值
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

// 一个 agent 可以覆盖的全局设置：全局值（global）、自己的值（value，null 表示跟随全局）、实际生效的值
export type AgentSetting = {
  key: string;
  label: string;
  type: 'int' | 'float' | 'string' | 'enum';
  unit?: string;
  help?: string;
  options?: string[] | null;
  global: any;
  value: any;
  effective: any;
};

// 绑在这个 agent 上的一个机器人
export type AgentBot = { id: string; kind: string; name: string; channel: string; enabled: boolean };

// 一个 agent：名字、头像、人格、模型，以及它覆盖了什么设置
export type AgentInfo = {
  id: string;
  name: string;
  emoji: string;
  default: boolean;
  model: string;
  model_label: string;
  thinking: string;
  thinking_levels: string[] | null;
  tools: string[];
  group_tools: string[];
  tools_available: string[];
  bots: AgentBot[];
  sessions: number;
  running: boolean;
  persona_file: string;
  persona_chars: number;
  settings: AgentSetting[];
};

// 一个机器人：绑一个 agent，凭据只回传有没有（不返回明文）
// 网关内部的群聊（房间）：成员是 agent，聊天记录是一个会话（sid）
export type RoomInfo = {
  id: string;
  name: string;
  members: string[];
  mode: 'mention' | 'free';
  paused: boolean;
  muted: string[];
  dm: boolean;
  budget?: number;
  sid: string;
  count: number;
};

export type BotInfo = {
  id: string;
  kind: 'telegram' | 'feishu';
  name: string;
  app_id: string;
  agent: string;
  show: string;
  channel: string;
  enabled: boolean;
  legacy: boolean;
  has_token: boolean;
  has_secret: boolean;
  proxy: string;
  api_base: string;
  domain: string;
  group_all: boolean;
};

// 拓扑图里可以点开的对象。每个对象的面板就是它全部的状态、操作和设置（没有单独的设置页）
export type Target =
  | { type: 'gateway' }
  | { type: 'agent'; id: string } // 一个 agent：人格、模型、覆盖的设置、绑定的机器人
  | { type: 'agentDefaults' } // Agent 的全局默认值：所有 agent 没覆盖的项
  | { type: 'agents' } // ＋ 添加 agent
  | { type: 'bot'; id: string } // 一个机器人
  | { type: 'room'; id: string } // 网关内部的群聊（房间）
  | { type: 'rooms' } // ＋ 创建群
  | { type: 'bots' } // ＋ 添加机器人
  | { type: 'model' }
  | { type: 'kernel'; node?: string } // 在线的内核（node 是某一个）
  | { type: 'machine'; name: string } // 一台机器：本机、装了内核的电脑、SSH 服务器
  | { type: 'machines' } // ＋ 接入机器：配对请求、安装命令、添加 SSH 服务器、电脑接入地址
  | { type: 'workspace'; id?: string } // 一个工作目录；没有 ID 时是添加工作目录
  | { type: 'channel'; name: string }
  | { type: 'session'; sid: string }
  | { type: 'search' }
  | { type: 'memory' }
  | { type: 'skills' }
  | { type: 'schedules' };

// /api/devices：SSH 连接、配对请求、配对过的电脑、电脑接入地址
export type SshConnection = {
  id: string; name: string; host: string; port: number; user: string; identity_file: string;
  has_password: boolean; auto_connect: boolean; state: string; detail?: string;
};
export type PairRequest = { id: string; node: string; addr: string; code: string; at: string };
export type PairedComputer = { node: string; addr: string; since: string; online: boolean };
export type Devices = {
  connections: SshConnection[]; ssh_hosts: string[]; listener: string; primary_listener: string; arch: string;
  pairing: PairRequest[]; paired: PairedComputer[]; addrs: string[]; download_base?: string;
};

export type Persona = { name: string; sid?: string; size: number; mod: number; sessions: string[] };
export type PersonaPreset = { id: string; name: string; desc: string; text: string };

export type ScheduleResult = { at: string; ok: boolean; summary: string };
export type ScheduleTask = {
 id: string; name: string; when: string; agent: string; prompt: string; deliver: string[]; workspaces: string[];
 mode: string; creator_mode: string; enabled: boolean; catch_up: boolean; created_by: string; timeout?: string;
 next: string; running: boolean; last?: ScheduleResult; results: ScheduleResult[];
};
export type ScheduleView = { tasks: ScheduleTask[]; timezone: string; error?: string };
