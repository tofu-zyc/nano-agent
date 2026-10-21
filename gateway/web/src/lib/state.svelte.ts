// 全局状态：网关的总览、配置、实时事件。通过 SSE 订阅网关的事件总线，界面上的一切都跟着实时变。
import { api, setUnauthorizedHandler, sidPath } from './api';
import type { Approval, Ev, FieldState, LiveItem, Overview, SessionInfo, Target } from './types';

export type Toast = {
  id: number;
  kind: 'info' | 'ok' | 'err' | 'approval';
  text: string;
  detail?: string;
  approval?: Approval;
};

export type View = 'topology' | 'sessions' | 'personas' | 'settings';
export type Route = { view: View; arg?: string; focus?: string };

function parseRoute(): Route {
  const h = location.hash.replace(/^#\/?/, '');
  const [path, query] = h.split('?');
  const [view, ...rest] = path.split('/');
  const params = new URLSearchParams(query || '');
  const arg = rest.length && rest[0] ? decodeURIComponent(rest.join('/')) : undefined;
  if (view === 'sessions' || view === 'personas' || view === 'settings') {
    return { view, arg, focus: params.get('focus') ?? undefined };
  }
  return { view: 'topology' };
}

type Theme = 'auto' | 'dark' | 'light';

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
  phase = $state<Record<string, string>>({}); // 会话正在做什么：thinking / writing / tool
  log = $state<Ev[]>([]);
  toasts = $state<Toast[]>([]);
  inspector = $state<Target | null>(null);
  focus = $state<string | null>(null); // 设置面板里要高亮的配置项
  private pendingOpen: { t: Target; focus: string | null } | null = null;
  palette = $state(false);
  route = $state<Route>(parseRoute());
  theme = $state<Theme>(loadTheme());
  connected = $state(false);
  historyTick = $state<Record<string, number>>({});
  personaTick = $state(0);
  feishuReg = $state<{ kind: string; text: string } | null>(null); // 扫码创建飞书机器人的结果

  private es: EventSource | null = null;
  private listeners = new Set<(e: Ev) => void>();
  private toastSeq = 0;

  constructor() {
    window.addEventListener('hashchange', () => {
      const prev = this.route.view;
      this.route = parseRoute();
      // 换了页面：右侧的设置面板关掉，除非是“去某个页面并打开某个对象”
      if (this.route.view !== prev) {
        this.inspector = this.pendingOpen?.t ?? null;
        this.focus = this.pendingOpen?.focus ?? null;
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

  // ---- 登录和启动 ----

  async boot() {
    // 启动链接里带着密码（#k=…）：自动登录，再把它从地址栏去掉
    const m = location.hash.match(/[#&]k=([^&]+)/);
    if (m) {
      history.replaceState(null, '', location.pathname + '#/');
      this.route = parseRoute();
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
    this.connect();
  }

  async refresh() {
    this.ov = await api.get<Overview>('/api/overview');
  }

  async loadFields() {
    const r = await api.get<{ fields: FieldState[] }>('/api/config');
    this.fields = r.fields;
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

  session(sid: string | undefined) {
    return sid ? this.ov?.sessions?.find((s) => s.sid === sid) : undefined;
  }

  upsertSession(info: SessionInfo) {
    if (!this.ov) return;
    if (!this.ov.sessions) this.ov.sessions = [];
    const list = this.ov.sessions;
    const i = list.findIndex((s) => s.sid === info.sid);
    if (i >= 0) list[i] = info;
    else list.unshift(info);
  }

  async refreshSession(sid: string) {
    try {
      const r = await api.get<{ info: SessionInfo }>(sidPath(sid));
      this.upsertSession(r.info);
    } catch {
      /* 忽略 */
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
        if (ov) ov.kernel = e.data ?? null;
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
      case 'message':
        if (!this.session(sid)) this.refreshSession(sid);
        this.pushLive(sid, { kind: 'user', text: e.text ?? '', name: e.name, at: e.at });
        break;
      case 'run': {
        const s = this.session(sid);
        if (e.kind === 'start') {
          if (s) s.running = true;
          this.phase[sid] = 'thinking';
          if (ov) ov.totals.runs++;
        } else {
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
        const list = this.liveOf(sid);
        const last = list[list.length - 1];
        const kind = e.kind === 'reasoning' ? 'reasoning' : 'content';
        if (last && last.kind === kind) last.text += e.text ?? '';
        else list.push({ kind, text: e.text ?? '' });
        this.phase[sid] = kind === 'reasoning' ? 'thinking' : 'writing';
        break;
      }
      case 'tool':
        this.pushLive(sid, { kind: 'tool', id: e.id!, name: e.name!, detail: e.text ?? '', args: e.data });
        this.phase[sid] = 'tool';
        break;
      case 'tool_result': {
        const it = this.live[sid]?.find((x) => x.kind === 'tool' && x.id === e.id);
        if (it && it.kind === 'tool') {
          it.ok = e.ok;
          it.result = e.data;
          it.resultDetail = e.text;
        }
        break;
      }
      case 'say':
        this.pushLive(sid, { kind: 'say', text: e.text ?? '', at: e.at });
        break;
      case 'approval': {
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
    }
    if (e.t !== 'delta') {
      this.log.push(e);
      if (this.log.length > 400) this.log.splice(0, this.log.length - 300);
    }
    for (const f of this.listeners) f(e);
  }

  // ---- 操作 ----

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
