// 全局状态：网关的总览、配置、实时事件。通过 SSE 订阅网关的事件总线，界面上的一切都跟着实时变。
import { api, ApiError, setUnauthorizedHandler, sidPath } from './api';
import type { AgentInfo, Approval, BotInfo, Devices, Ev, FieldState, LiveItem, MachineInfo, ModelsView, Overview, PairRequest, RoomInfo, SessionInfo, Target, WorkspaceInfo, UpdateInfo } from './types';
import { HOMES } from './nodes';

export type Toast = {
  id: number;
  kind: 'info' | 'ok' | 'err' | 'approval' | 'pair';
  text: string;
  detail?: string;
  approval?: Approval;
  pair?: PairRequest;
  open?: string; // 点“去看看”打开的会话（拟人型主动找你时）
};

export type { PairRequest };

// 只有两个页面：拓扑图（所有对象的状态、操作和设置都在节点上）和会话（聊天）
export type View = 'topology' | 'sessions';
export type Route = { view: View; arg?: string };

// 以前的页面（设置、设备、定时任务、人格）的链接：跳到拓扑图，打开对应的节点
type Legacy = { target: Target; focus?: string; persona?: { name: string; isNew: boolean; agent?: string } };

function parseHash(): { route: Route; legacy?: Legacy } {
  const h = location.hash.replace(/^#\/?/, '');
  const [path, query] = h.split('?');
  const [view, ...rest] = path.split('/');
  const params = new URLSearchParams(query || '');
  const arg = rest.length && rest[0] ? decodeURIComponent(rest.join('/')) : undefined;
  const topo: Route = { view: 'topology' };
  switch (view) {
    case 'sessions':
      return { route: { view, arg } };
    case 'settings':
      return { route: topo, legacy: { target: { type: 'gateway' }, focus: params.get('focus') ?? undefined } };
    case 'devices':
      return { route: topo, legacy: { target: { type: 'machines' } } };
    case 'schedules':
      return { route: topo, legacy: { target: { type: 'schedules' } } };
    case 'personas': {
      // 人格现在是每个 agent 自己的：默认人格是默认 agent 的 persona.md，其他的是会话专属
      const name = arg ?? 'default.md';
      if (name === 'default.md') {
        return { route: topo, legacy: { target: { type: 'agent', id: 'default' }, persona: { name: 'persona.md', isNew: false, agent: 'default' } } };
      }
      return { route: topo, legacy: { target: { type: 'agent', id: 'default' }, persona: { name, isNew: params.get('new') === '1' } } };
    }
  }
  return { route: topo };
}



type Theme = 'auto' | 'dark' | 'light';

function loadWidth(): number {
  try {
    const w = Number(localStorage.getItem('nano.panel'));
    if (w >= 380) return w;
  } catch {
    /* 隐私模式之类，忽略 */
  }
  return 440;
}

function loadTheme(): Theme {
  try {
    const t = localStorage.getItem('nano.theme');
    if (t === 'dark' || t === 'light') return t;
  } catch {
    /* 隐私模式之类，忽略 */
  }
  return 'auto';
}

class Store {
  authed = $state<boolean | null>(null);
  ov = $state<Overview | null>(null);
  fields = $state<FieldState[]>([]);
  live = $state<Record<string, LiveItem[]>>({});
  activeRun = $state<Record<string,string>>({});
  closedRun = $state<Record<string,string>>({});
  phase = $state<Record<string, string>>({}); // 会话正在做什么：thinking / writing / tool
  log = $state<Ev[]>([]);
  toasts = $state<Toast[]>([]);
  inspector = $state<Target | null>(null);
  focus = $state<string | null>(null); // 节点面板里要高亮的配置项
  private pendingOpen: { t: Target; focus: string | null } | null = null;
  palette = $state(false);
  modelIntent = $state<'model' | 'endpoint' | null>(null); // 打开模型面板后直接进添加模型/接口
  route = $state<Route>(parseHash().route);
  devices = $state<Devices | null>(null); // SSH 连接、配对请求、配对过的电脑（机器节点和“接入机器”用）
  persona = $state<{ name: string; isNew: boolean; agent?: string } | null>(null); // 正在编辑的人格文件（agent 在时是那个 agent 自己的）
  scheduleNew = $state(false); // 打开定时任务节点后直接新建
  panelWidth = $state(loadWidth());
  theme = $state<Theme>(loadTheme());
  connected = $state(false);
  historyTick = $state<Record<string, number>>({});
  personaTick = $state(0);
  memoryTick = $state(0); // 记忆、整理记忆、内心活动有变化（记忆面板据此重读）
  feishuReg = $state<{ kind: string; text: string } | null>(null); // 扫码创建飞书机器人的结果
  botReg = $state<{ kind: string; text: string; data?: any; n: number } | null>(null); // QQ / 微信扫码的进展（每条都是新的）
  restarting = $state(false);
  updating = $state(false);
  checkingUpdate = $state(false);

  private es: EventSource | null = null;
  private listeners = new Set<(e: Ev) => void>();
  private toastSeq = 0;

  constructor() {
    window.addEventListener('hashchange', () => {
      const prev = this.route;
      const { route, legacy } = parseHash();
      this.route = route;
      if (legacy) return this.openLegacy(legacy);
      // 换了页面：右侧的面板关掉，除非是“去某个页面并打开某个对象”
      if (this.route.view !== prev.view) {
        this.inspector = this.pendingOpen?.t ?? null;
        this.focus = this.pendingOpen?.focus ?? null;
      } else if (this.route.view === 'sessions' && this.route.arg !== prev.arg && this.inspector?.type === 'session') {
        // 在会话列表里切换会话：面板跟着换成当前会话；没选会话就收起
        this.inspector = this.route.arg ? { type: 'session', sid: this.route.arg } : null;
        this.focus = null;
      }
      this.pendingOpen = null;
    });
    setUnauthorizedHandler(() => {
      this.authed = false;
      this.es?.close();
    });
    this.applyTheme();
  }

  go(hash: string) {
    location.hash = hash;
  }

  /** 以前页面的链接：换成拓扑图上的节点 */
  private openLegacy(l: Legacy) {
    history.replaceState(null, '', location.pathname + '#/');
    this.route = { view: 'topology' };
    // 设置项按它的 group 找到所在的节点
    const f = l.focus ? this.fields.find((x) => x.key === l.focus) : undefined;
    const t = (f && HOMES[f.group]?.target) || l.target;
    this.inspector = t;
    this.focus = l.focus ?? null;
    if (l.persona) this.persona = l.persona;
  }

  /** 打开模型节点面板，可选直接进添加模型/接口 */
  openModel(intent: 'model' | 'endpoint' | null = null) {
    this.openOnMap({ type: 'model' });
    this.modelIntent = intent;
  }

  // ---- 登录和启动 ----

  async boot() {
    // 启动链接里带着密码（#k=…）：自动登录，再把它从地址栏去掉
    const m = location.hash.match(/[#&]k=([^&]+)/);
    if (m) {
      history.replaceState(null, '', location.pathname + '#/');
      this.route = parseHash().route;
      try {
        await api.post('/api/login', { password: decodeURIComponent(m[1]) });
      } catch {
        /* 密码过期了就走正常登录 */
      }
    }
    try {
      await api.get('/api/me');
      this.authed = true;
      await this.start();
    } catch {
      this.authed = false;
    }
  }

  async login(password: string) {
    await api.post('/api/login', { password });
    this.authed = true;
    await this.start();
  }

  async logout() {
    await api.post('/api/logout').catch(() => {});
    this.es?.close();
    this.authed = false;
  }

  private async start() {
    await Promise.all([this.refresh(), this.loadFields()]);
    // 因为网关换了版本而自动刷新的：告诉一声
    try {
      const v = sessionStorage.getItem('nano.updated');
      if (v) {
        sessionStorage.removeItem('nano.updated');
        this.toast({ kind: 'ok', text: `网关已经是 ${v}，页面已刷新成新版本` }, 4000);
      }
    } catch {
      /* 忽略 */
    }
    const { legacy } = parseHash();
    if (legacy) this.openLegacy(legacy);
    this.connect();
    this.checkPairing(); // 打开网页前就有电脑在等确认
  }

  private reloadTimer: ReturnType<typeof setTimeout> | undefined;
  private agentsTimer: ReturnType<typeof setTimeout> | undefined;

  /** agent 的状态（在干嘛）跟着会话变：过一会儿只重读 agent 列表 */
  reloadAgentsSoon() {
    clearTimeout(this.agentsTimer);
    this.agentsTimer = setTimeout(async () => {
      try {
        const r = await api.get<{ items: AgentInfo[] }>('/api/agents');
        if (this.ov) this.ov.agents = r.items;
      } catch {
        /* 下次整体刷新时再读 */
      }
    }, 800);
  }

  // 这个页面加载时网关的版本。网关更新以后（这个标签页一直开着），页面代码还是旧的，
  // 新版本加的设置项、节点在旧页面上看不到：发现版本变了就自己刷新一次。
  private loadedVersion = '';

  async refresh() {
    const ov = await api.get<Overview>('/api/overview');
    const v = ov.runtime?.version ?? '';
    if (v && this.loadedVersion && v !== this.loadedVersion) {
      try {
        sessionStorage.setItem('nano.updated', v);
      } catch {
        /* 忽略 */
      }
      location.reload();
      return;
    }
    if (v) this.loadedVersion = v;
    this.ov = ov;
  }

  /** 重启网关。成功后页面等它回来，自动刷新；配置有问题时 API 会直接报错，网关保持运行。 */
  async restartGateway(sid = '') {
    this.restarting = true;
    try {
      await api.post('/api/restart', { sid });
    } catch (err) {
      this.restarting = false;
      this.toast({ kind: 'err', text: err instanceof Error ? err.message : String(err) }, 8000);
      return;
    }
    if (await this.waitBack()) this.toast({ kind: 'ok', text: '网关已重启' }, 3000);
  }

  /** 等重启中的网关回来，回来后重新加载数据；等太久就刷新页面。 */
  private async waitBack(): Promise<boolean> {
    for (let i = 0; i < 120; i++) {
      await new Promise((r) => setTimeout(r, 500));
      try {
        await api.get('/api/me');
        await this.start();
        this.restarting = false;
        return true;
      } catch {
        /* 还在重启 */
      }
    }
    this.restarting = false;
    location.reload();
    return false;
  }

  /** 查一次有没有新版本 */
  async checkUpdate() {
    this.checkingUpdate = true;
    try {
      const u = await api.post<UpdateInfo>('/api/update/check', {});
      await this.refresh();
      this.toast({ kind: 'ok', text: u.available ? `有新版本 ${u.latest}` : `已经是最新版本 ${u.current}` }, 3000);
    } catch (err) {
      this.toast({ kind: 'err', text: err instanceof Error ? err.message : String(err) }, 8000);
    } finally {
      this.checkingUpdate = false;
    }
  }

  /** 更新网关：下载、校验、换好后网关重启；页面等它回来。新版本起不来会自动退回，聊天里会说明。 */
  async updateGateway(sid = '') {
    const before = this.ov?.runtime?.version ?? '';
    this.updating = true;
    try {
      await api.post('/api/update', { sid });
    } catch (err) {
      this.updating = false;
      this.toast({ kind: 'err', text: err instanceof Error ? err.message : String(err) }, 8000);
      return;
    }
    this.restarting = true;
    // 新版本要正常运行一会儿才算成功；先等它回来，版本号变了就是换上了
    const back = await this.waitBack();
    this.updating = false;
    if (!back) return;
    const now = this.ov?.runtime?.version ?? '';
    if (now && now !== before) this.toast({ kind: 'ok', text: `网关已更新到 ${now}` }, 4000);
    else this.toast({ kind: 'err', text: '新版本没能正常运行，已退回；详情看网关日志' }, 8000);
  }

  async loadFields() {
    const r = await api.get<{ fields: FieldState[] }>('/api/config');
    this.fields = r.fields;
  }

  /** 重新读模型列表（端点、模型、默认模型） */
  async reloadModels() {
    try {
      const m = await api.get<ModelsView>('/api/models');
      if (this.ov) this.ov.models = m;
    } catch {
      /* 下次刷新再说 */
    }
  }

  private connect() {
    this.es?.close();
    const es = new EventSource('/api/events');
    this.es = es;
    let reopened = false;
    es.onopen = () => {
      this.connected = true;
      if (reopened) this.refresh().catch(() => {});
    };
    es.onerror = () => {
      this.connected = false;
      reopened = true;
      api.get('/api/me').catch(() => {}); // 登录过期的话会切回登录页
    };
    es.onmessage = (m) => {
      try {
        this.handle(JSON.parse(m.data));
      } catch (err) {
        console.error(err);
      }
    };
  }

  /** 订阅原始事件（拓扑图的动画用） */
  on(f: (e: Ev) => void) {
    this.listeners.add(f);
    return () => this.listeners.delete(f);
  }

  // ---- 会话 ----

  /** 所有 agent */
  get agents(): AgentInfo[] {
    return this.ov?.agents ?? [];
  }

  /** 所有机器人 */
  get bots(): BotInfo[] {
    return this.ov?.bots ?? [];
  }

  /** 网关内部的群聊（房间） */
  get rooms(): RoomInfo[] {
    return this.ov?.rooms ?? [];
  }

  room(id: string | undefined): RoomInfo | undefined {
    return id ? this.rooms.find((r) => r.id === id) : undefined;
  }

  agent(id: string | undefined): AgentInfo | undefined {
    return id ? this.agents.find((a) => a.id === id) : undefined;
  }

  /** 绑在某个渠道上的机器人（内置的 tg/feishu 也在这里） */
  botOfChannel(channel: string): BotInfo | undefined {
    return this.bots.find((b) => b.channel === channel);
  }

  /** 所有机器：本机、装了内核的电脑、SSH 服务器 */
  get machines(): MachineInfo[] {
    return this.ov?.machines ?? [];
  }

  /** 机器 name 上的工作目录 */
  workspacesOn(name: string): WorkspaceInfo[] {
    return (this.ov?.workspaces ?? []).filter((w) => w.host === name);
  }

  session(sid: string | undefined) {
    return sid ? this.ov?.sessions?.find((s) => s.sid === sid) : undefined;
  }

  upsertSession(info: SessionInfo) {
    if (/^@[^/]+\/room:/.test(info.sid)) return; // 内部成员只在公共群记录里展示
    if (!this.ov) return;
    if (!this.ov.sessions) this.ov.sessions = [];
    const list = this.ov.sessions;
    const i = list.findIndex((s) => s.sid === info.sid);
    if (i >= 0) list[i] = info;
    else list.unshift(info);
  }

  /** 从网关重新读一个会话的信息。会话已经不存在时从列表里去掉，返回 false */
  async refreshSession(sid: string) {
    try {
      const r = await api.get<{ info: SessionInfo }>(sidPath(sid));
      this.upsertSession(r.info);
      return true;
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) {
        this.dropSession(sid);
        return false;
      }
      return true;
    }
  }

  private dropSession(sid: string) {
    if (this.ov?.sessions) this.ov.sessions = this.ov.sessions.filter((s) => s.sid !== sid);
    delete this.live[sid];
    delete this.phase[sid];
  }

  /** 新建一个会话并打开它（agent 留空就是默认 agent） */
  async newSession(title = '', agent = '') {
    try {
      const info = await api.post<SessionInfo>('/api/sessions', { title, agent });
      this.upsertSession(info);
      location.hash = '#/sessions/' + encodeURIComponent(info.sid);
      return info;
    } catch (err) {
      this.fail(err);
    }
  }

  // 注意：不能写成 (this.live[sid] ??= []).push(…)。??= 返回的是原始数组而不是状态里的代理，
  // 第一次写进去的内容界面上看不到
  private liveOf(sid: string): LiveItem[] {
    if (!this.live[sid]) this.live[sid] = [];
    return this.live[sid];
  }

  private pushLive(sid: string, item: LiveItem) {
    this.liveOf(sid).push(item);
  }

  // ---- 事件 ----

  handle(e: Ev) {
    const ov = this.ov;
    const sid = e.sid ?? '';
    switch (e.t) {
      case 'kernel':
        if (ov) {
          ov.kernels = ov.kernels.filter((k) => k.node !== e.name);
          if (e.data) ov.kernels = [...ov.kernels, e.data].sort((a, b) => a.node.localeCompare(b.node));
        }
        this.loadDevicesSoon();
        break;
      case 'machines':
        // 机器能不能用变了：会话里工作目录怎么执行也跟着变，过一会儿整体重新读一遍
        if (ov && e.data) {
          ov.machines = e.data;
          clearTimeout(this.reloadTimer);
          this.reloadTimer = setTimeout(() => this.refresh().catch(() => {}), 400);
        }
        this.loadDevicesSoon();
        break;
      case 'skills':
        if (ov && e.data) ov.skills = { ...ov.skills, list: e.data };
        break;
      case 'execution_leases':
      case 'mcp':
        // MCP 服务改了配置、连上或断开：重新读一遍概况（拓扑图上的节点、面板里的在线状态）
        clearTimeout(this.reloadTimer);
        this.reloadTimer = setTimeout(() => this.refresh().catch(() => {}), 400);
        break;
      case 'models':
        this.reloadModels();
        break;
      case 'reach':
        // 拟人型主动发来一条私聊：正在看那个会话就不用弹了
        if (!(this.route.view === 'sessions' && this.route.arg === sid)) {
          this.toast({ kind: 'info', text: `${e.name ?? '它'} 来找你了`, detail: e.text ?? '', open: sid }, 12000);
        }
        break;
      case 'autonomy':
        // 自主活动状态变了：agent 节点上的日程、目标、想法跟着刷新
        this.memoryTick++;
        this.reloadAgentsSoon();
        break;
      case 'memory':
      case 'dream':
      case 'life':
        // 记忆变了、开始/结束整理、内心活动醒了：记忆面板重读，agent 节点上的状态跟着变
        this.memoryTick++;
        clearTimeout(this.reloadTimer);
        this.reloadTimer = setTimeout(() => this.refresh().catch(() => {}), 300);
        break;
      case 'agents':
      case 'bots':
      case 'rooms':
        // agent 或机器人列表变了：拓扑图和面板跟着重新读一遍
        clearTimeout(this.reloadTimer);
        this.reloadTimer = setTimeout(() => this.refresh().catch(() => {}), 300);
        break;
      case 'search':
        if (ov && e.data) ov.search = e.data;
        break;
      case 'schedules':
        if (ov && e.data) ov.schedules = e.data;
        break;
      case 'context':
        // 整理完上下文：记忆节点上“整理过的会话”跟着变
        if (e.kind === 'end') {
          clearTimeout(this.reloadTimer);
          this.reloadTimer = setTimeout(() => this.refresh().catch(() => {}), 400);
        }
        break;
      case 'workspaces':
        if (ov && e.data) ov.workspaces = e.data;
        break;
      case 'channel':
        if (ov && e.data) {
          const i = ov.channels.findIndex((c) => c.name === e.name);
          if (i >= 0) ov.channels[i] = e.data;
          else ov.channels.push(e.data);
        }
        break;
      case 'session':
        if (e.data) this.upsertSession(e.data);
        break;
      case 'session_deleted':
        this.dropSession(sid);
        if (this.inspector?.type === 'session' && this.inspector.sid === sid) this.inspector = null;
        if (this.route.view === 'sessions' && this.route.arg === sid) location.hash = '#/sessions';
        break;
      case 'message':
        if (!this.session(sid)) this.refreshSession(sid);
        if (sid.startsWith('room:')) {
          this.historyTick[sid] = (this.historyTick[sid] ?? 0) + 1;
          break;
        }
        this.pushLive(sid, { kind: 'user', text: e.text ?? '', name: e.name, via: e.data?.via, channel: e.data?.channel, images: e.data?.images, at: e.at });
        break;
      case 'pairing':
        this.checkPairing();
        break;
      case 'undo': {
        // 撤回了排队中的消息：去掉实时显示的那条；撤回了一轮：从历史重新读
        const list = this.live[sid];
        if (e.data?.queued) {
          const i = list ? list.map((x) => x.kind === 'user' && x.text === e.text).lastIndexOf(true) : -1;
          if (list && i >= 0) list.splice(i, 1);
        } else {
          if (!this.session(sid)?.running) delete this.live[sid];
          this.historyTick[sid] = (this.historyTick[sid] ?? 0) + 1;
        }
        break;
      }
      case 'run': {
        this.reloadAgentsSoon();
        const s = this.session(sid);
        if (e.kind === 'start') {
          if(e.run_id) this.activeRun[sid]=e.run_id;
          if (s) s.running = true;
          this.phase[sid] = 'thinking';
          if (ov) ov.totals.runs++;
        } else {
          if(e.run_id && this.activeRun[sid] && this.activeRun[sid]!==e.run_id) break;
          if(e.run_id) this.closedRun[sid]=e.run_id;
          if (s) s.running = false;
          delete this.phase[sid];
          this.historyTick[sid] = (this.historyTick[sid] ?? 0) + 1;
          this.refreshSession(sid);
          // 没在看这个会话：实时内容不用留着了（看的时候会从历史里读）
          if (!(this.route.view === 'sessions' && this.route.arg === sid)) delete this.live[sid];
        }
        break;
      }
      case 'model':
        if (e.kind === 'start') {
          this.phase[sid] = 'thinking';
          if (ov) ov.totals.model_calls++;
        } else if (ov && e.data?.usage) {
          const u = e.data.usage;
          ov.totals.prompt_tokens += u.prompt_tokens || 0;
          ov.totals.cached_tokens += u.cached_tokens || 0;
          ov.totals.completion_tokens += u.completion_tokens || 0;
          ov.totals.reasoning_tokens += u.reasoning_tokens || 0;
        }
        break;
      case 'delta': {
        if(e.run_id && (this.closedRun[sid]===e.run_id || (this.activeRun[sid] && this.activeRun[sid]!==e.run_id))) break;
        const list = this.liveOf(sid);
        const last = list[list.length - 1];
        const kind = e.kind === 'reasoning' ? 'reasoning' : 'content';
        if (last && last.kind === kind) last.text += e.text ?? '';
        else list.push({ kind, text: e.text ?? '' });
        this.phase[sid] = kind === 'reasoning' ? 'thinking' : 'writing';
        break;
      }
      case 'tool':
        this.pushLive(sid, { kind: 'tool',run_id:e.run_id,turn_id:e.turn_id,state:'accepted', id: e.id!, name: e.name!, detail: e.text ?? '', args: e.data });
        this.phase[sid] = 'tool';
        break;
      case 'tool_queued':
      case 'tool_started': {
        const it=this.live[sid]?.slice().reverse().find(x=>x.kind==='tool'&&x.id===e.id&&(!e.run_id||x.run_id===e.run_id)&&(!e.turn_id||x.turn_id===e.turn_id));
        if(it?.kind==='tool' && it.ok===undefined) it.state=e.t==='tool_queued'?'queued':'running';
        break;
      }
      case 'tool_result': {
        const it = this.live[sid]?.slice().reverse().find(x => x.kind === 'tool' && x.id === e.id && (!e.run_id || x.run_id === e.run_id) && (!e.turn_id || x.turn_id === e.turn_id));
        if (it && it.kind === 'tool') {
          it.state='done';
          it.ok = e.ok;
          it.result = e.data;
          it.resultDetail = e.text;
          it.sandbox = e.sandbox;
        }
        break;
      }
      case 'tool_media': {
        // 工具读到的图片、SendFile 发出去的文件：显示在那个工具卡片下面
        const it = this.live[sid]?.slice().reverse().find(x => x.kind === 'tool' && x.id === e.id && (!e.run_id || x.run_id === e.run_id) && (!e.turn_id || x.turn_id === e.turn_id));
        if (it && it.kind === 'tool') it.media = [...(it.media ?? []), ...(e.data?.images ?? []), ...(e.data?.sent ?? [])];
        break;
      }
      case 'say':
        if (sid.startsWith('room:')) {
          this.historyTick[sid] = (this.historyTick[sid] ?? 0) + 1;
          break;
        }
        this.pushLive(sid, { kind: 'say', text: e.text ?? '', at: e.at });
        break;
      case 'approval': {
        const it=this.live[sid]?.slice().reverse().find(x=>x.kind==='tool'&&x.id===e.id&&(!e.run_id||x.run_id===e.run_id)&&(!e.turn_id||x.turn_id===e.turn_id));
        if(it?.kind==='tool' && it.ok===undefined)it.state='waiting_approval';
        const ap: Approval = { n: e.n!, sid, id: e.id!, name: e.name!, detail: e.text ?? '', from: e.data?.from ?? '', at: e.at };
        ov?.approvals.push(ap);
        const s = this.session(sid);
        if (s) s.pending++;
        this.toast({ kind: 'approval', text: `需要审批 #${ap.n}`, approval: ap }, 0);
        break;
      }
      case 'approval_done': {
        if (ov) ov.approvals = ov.approvals.filter((a) => a.n !== e.n);
        const s = this.session(sid);
        if (s && s.pending > 0) s.pending--;
        this.toasts = this.toasts.filter((t) => t.approval?.n !== e.n);
        break;
      }
      case 'config':
        this.loadFields().catch(() => {});
        this.refresh().catch(() => {});
        break;
      case 'persona':
        this.personaTick++;
        break;
      case 'feishu_reg':
        this.feishuReg = { kind: e.kind ?? '', text: e.text ?? '' };
        break;
      case 'bot_reg':
        this.botReg = { kind: e.kind ?? '', text: e.text ?? '', data: e.data, n: (this.botReg?.n ?? 0) + 1 };
        break;
    }
    if (e.t !== 'delta') {
      this.log.push(e);
      if (this.log.length > 400) this.log.splice(0, this.log.length - 300);
    }
    for (const f of this.listeners) f(e);
  }

  // ---- 操作 ----

  /** 重新读 SSH 连接、配对请求、配对过的电脑 */
  async loadDevices() {
    try {
      this.devices = await api.get<Devices>('/api/devices');
    } catch {
      /* 下次再说 */
    }
    return this.devices;
  }

  private devicesTimer: ReturnType<typeof setTimeout> | undefined;
  private loadDevicesSoon() {
    clearTimeout(this.devicesTimer);
    this.devicesTimer = setTimeout(() => this.loadDevices(), 300);
  }

  /** 设置面板宽度（拖面板左边缘），记在浏览器里 */
  setPanelWidth(w: number) {
    this.panelWidth = Math.round(Math.min(Math.max(w, 380), Math.max(380, window.innerWidth - 160)));
    try {
      localStorage.setItem('nano.panel', String(this.panelWidth));
    } catch {
      /* 忽略 */
    }
  }

  // 有电脑请求配对：弹一个带“允许/拒绝”的提示，「接入机器」节点上也有标记；处理掉的请求收起提示
  private pairToasts = new Map<string, number>();
  async checkPairing() {
    const d = await this.loadDevices();
    if (!d) return;
    const reqs: PairRequest[] = d.pairing ?? [];
    for (const [id, tid] of this.pairToasts) {
      if (!reqs.some((r) => r.id === id)) {
        this.dismiss(tid);
        this.pairToasts.delete(id);
      }
    }
    for (const r of reqs) {
      if (!this.pairToasts.has(r.id)) this.pairToasts.set(r.id, this.toast({ kind: 'pair', text: `${r.node} 请求接入`, pair: r }, 0));
    }
  }

  async decidePair(r: PairRequest, ok: boolean) {
    try {
      await api.post(`/api/pairing/${r.id}`, { ok });
    } catch (err) {
      this.fail(err);
    }
    this.checkPairing();
  }

  toast(t: Omit<Toast, 'id'>, ms = 3200) {
    const id = ++this.toastSeq;
    this.toasts.push({ ...t, id });
    if (ms > 0) setTimeout(() => this.dismiss(id), ms);
    return id;
  }

  dismiss(id: number) {
    this.toasts = this.toasts.filter((t) => t.id !== id);
  }

  fail(err: unknown) {
    this.toast({ kind: 'err', text: err instanceof Error ? err.message : String(err) }, 6000);
  }

  async approve(n: number, ok: boolean) {
    try {
      await api.post(`/api/approvals/${n}`, { ok });
    } catch (err) {
      this.fail(err);
    }
  }

  /** 修改配置。成功返回 true；失败时弹出原因 */
  async setConfig(values: Record<string, unknown>, quiet = false) {
    try {
      const r = await api.patch<{ fields: FieldState[] }>('/api/config', { values });
      this.fields = r.fields;
      if (!quiet) this.toast({ kind: 'ok', text: '已保存' }, 1600);
      return true;
    } catch (err) {
      this.fail(err);
      return false;
    }
  }

  field(key: string) {
    return this.fields.find((f) => f.key === key);
  }

  open(t: Target, focus: string | null = null) {
    this.inspector = t;
    this.focus = focus;
  }

  /** 去拓扑图并打开某个对象 */
  openOnMap(t: Target, focus: string | null = null) {
    if (this.route.view === 'topology') return this.open(t, focus);
    this.pendingOpen = { t, focus };
    location.hash = '#/';
  }

  // ---- 主题 ----

  setTheme(t: Theme) {
    this.theme = t;
    try {
      if (t === 'auto') localStorage.removeItem('nano.theme');
      else localStorage.setItem('nano.theme', t);
    } catch {
      /* 忽略 */
    }
    this.applyTheme();
  }

  private applyTheme() {
    if (this.theme === 'auto') document.documentElement.removeAttribute('data-theme');
    else document.documentElement.setAttribute('data-theme', this.theme);
  }

  get dark() {
    if (this.theme !== 'auto') return this.theme === 'dark';
    return !window.matchMedia('(prefers-color-scheme: light)').matches;
  }
}

export const store = new Store();

// 排查问题时在浏览器控制台里看状态：__nano.live、__nano.log
(window as any).__nano = store;
