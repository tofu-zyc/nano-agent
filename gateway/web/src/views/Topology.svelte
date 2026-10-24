<script lang="ts">
  // 拓扑图就是控制台：每个能配置的东西都是一个节点，点开就是它全部的状态、操作和设置。
  // 从左到右：入口 → 会话 → Agent → 网关 → 能力（记忆、搜索、技能、模型）/ 内核 → 机器 → 工作目录。
  import { onMount } from 'svelte';
  import { store } from '../lib/state.svelte';
  import { api, sidPath } from '../lib/api';
  import { ago, channelMeta, homeShort, machineMeta, modeMeta, num, osLabel, pct, sessionChannels, shortPath, stateLabel, truncate, whenText } from '../lib/format';
  import { portal } from '../lib/portal';
  import Icon from '../components/Icon.svelte';
  import ContextMenu, { type MenuItem } from '../components/ContextMenu.svelte';
  import WorkspacePicker from '../components/WorkspacePicker.svelte';
  import DirPicker from '../components/DirPicker.svelte';
  import type { AgentInfo, Ev, KernelInfo, MachineInfo, MemoryInfo, MountInfo, RoomInfo, ScheduleSummary, SearchInfo, SessionInfo, Target, WorkspaceInfo } from '../lib/types';

  type Kind = 'channel' | 'schedule' | 'session' | 'agent' | 'room' | 'agentdefaults' | 'botadd' | 'gateway' | 'model' | 'search' | 'memory' | 'skills' | 'kernel' | 'machine' | 'workspace' | 'more' | 'add';
  type Col = 'in' | 'sess' | 'agent' | 'gw' | 'caps' | 'mach' | 'ws';
  type N = { id: string; kind: Kind; col: Col; x: number; y: number; w: number; h: number; color: string; target?: Target; data?: any };
  type E = { id: string; from: string; to: string; color: string; dashed?: boolean; flow?: boolean };
  // 节点上的标记：有问题要处理（err / warn），或者有新东西（info）
  type Issue = { level: 'err' | 'warn' | 'info'; text: string; n?: number };

  const MAX_SESSIONS = 9;
  const ORDER = ['cli', 'web', 'telegram', 'feishu'];
  const X = { chan: 0, sess: 280, agent: 570, gw: 880, right: 1030, mach: 1360, ws: 1680 };
  const COLS: { col: Col; x: number; w: number; label: string }[] = [
    { col: 'in', x: X.chan, w: 200, label: '入口' },
    { col: 'sess', x: X.sess, w: 220, label: '会话' },
    { col: 'agent', x: X.agent, w: 210, label: 'Agent' },
    { col: 'gw', x: X.gw - 62, w: 124, label: '网关' },
    { col: 'caps', x: X.right, w: 250, label: '能力和内核' },
    { col: 'mach', x: X.mach, w: 250, label: '机器' },
    { col: 'ws', x: X.ws, w: 210, label: '工作目录' },
  ];

  // 拖动过的节点记下偏移量（存在浏览器里，下次打开还是这样摆）
  function loadOffsets(): Record<string, { x: number; y: number }> {
    try {
      return JSON.parse(localStorage.getItem('nano.layout2') || '{}');
    } catch {
      return {};
    }
  }
  let offsets = $state<Record<string, { x: number; y: number }>>(loadOffsets());
  function saveOffsets() {
    try {
      localStorage.setItem('nano.layout2', JSON.stringify(offsets));
    } catch {
      /* 忽略 */
    }
  }
  function resetLayout() {
    offsets = {};
    saveOffsets();
    fit();
  }


  // 正在跑任务的内核（连线上有流动的效果）
  const busyKernels = $derived(new Set((store.ov?.sessions ?? []).filter((s) => s.running).map((s) => s.run_node)));
  const anyRunning = $derived((store.ov?.sessions ?? []).some((s) => s.running));

  const layout = $derived.by(() => {
    const ov = store.ov!;
    const nodes: N[] = [];
    const edges: E[] = [];

    const all = (ov.sessions ?? []).slice();
    // 先挑出最值得看的会话（在跑的、待审批的、最近活跃的），再按渠道排，连线少交叉
    all.sort((a, b) => Number(b.running || b.pending > 0) - Number(a.running || a.pending > 0) || b.last_at - a.last_at);
    const shown = all.slice(0, MAX_SESSIONS);
    const rank = (s: SessionInfo) => (s.sid === 'main' ? -1 : s.sid.startsWith('cron:') ? 99 : ORDER.indexOf(sessionChannels(s)[0]));
    shown.sort((a, b) => rank(a) - rank(b) || b.last_at - a.last_at);
    const more = all.length - shown.length;

    const chans = ov.channels;
    const bots = ov.bots ?? [];
    const botOf = (name: string) => bots.find((b) => b.channel === name);
    const nS = shown.length + (more > 0 ? 1 : 0);
    const nL = chans.length + 2; // 渠道、定时任务，最下面是“添加机器人”
    const kernels: KernelInfo[] = (ov.kernels ?? []).slice().sort((a, b) =>
      Number(!!b.managed) - Number(!!a.managed) || Number(b.local) - Number(a.local) || a.node.localeCompare(b.node));
    const nM = store.machines.length + 1; // 机器，最下面是“接入机器”
    const H = Math.max(900, nS * 64 + 160, nL * 96 + 160, 2 * (70 + Math.max(1, nM, kernels.length) * 96));
    const cy = H / 2;
    const leftY = (i: number) => cy + (i - (nL - 1) / 2) * 96;

    chans.forEach((c, i) => {
      const y = leftY(i);
      const bot = botOf(c.name);
      const meta = channelMeta[c.name] ?? { label: bot?.name ?? c.name, icon: bot?.kind === 'feishu' ? 'feishu' : 'send',
        color: bot?.kind === 'feishu' ? 'var(--c-feishu)' : 'var(--c-telegram)' };
      const target: Target = bot && !bot.legacy ? { type: 'bot', id: bot.id } : { type: 'channel', name: c.name };
      nodes.push({ id: 'c:' + c.name, kind: 'channel', col: 'in', x: X.chan, y: y - 32, w: 200, h: 64, color: meta.color,
        target, data: { ...c, meta } });
    });
    const sc = ov.schedules;
    nodes.push({ id: 'sched', kind: 'schedule', col: 'in', x: X.chan, y: leftY(chans.length) - 32, w: 200, h: 64, color: sc?.total ? 'var(--c-cron)' : 'var(--muted)',
      target: { type: 'schedules' }, data: sc });
    nodes.push({ id: 'add:bot', kind: 'botadd', col: 'in', x: X.chan, y: leftY(chans.length + 1) - 22, w: 200, h: 44, color: 'var(--c-telegram)',
      target: { type: 'bots' }, data: { icon: 'plus', title: '添加机器人', sub: '让某个 agent 有自己的机器人' } });

    // 会话连到它接着的窗口所在的渠道（一个会话可以接好几个渠道的窗口），再连到 Agent
    shown.forEach((s, j) => {
      const y = cy + (j - (nS - 1) / 2) * 64;
      const color = s.sid === 'main' ? 'var(--accent)' : (channelMeta[s.channel]?.color ?? 'var(--c-session)');
      nodes.push({ id: 's:' + s.sid, kind: 'session', col: 'sess', x: X.sess, y: y - 24, w: 220, h: 48, color,
        target: { type: 'session', sid: s.sid }, data: s });
      for (const ch of sessionChannels(s)) {
        const cc = channelMeta[ch]?.color ?? 'var(--c-session)';
        edges.push({ id: `c:${ch}>s:${s.sid}`, from: 'c:' + ch, to: 's:' + s.sid, color: cc, flow: s.running });
      }
      if (s.sid.startsWith('cron:')) edges.push({ id: `sched>s:${s.sid}`, from: 'sched', to: 's:' + s.sid, color: 'var(--c-cron)', flow: s.running });
      edges.push({ id: `s:${s.sid}>ag:${s.agent}`, from: 's:' + s.sid, to: 'ag:' + s.agent, color: 'var(--c-session)', flow: s.running });
    });
    if (more > 0) {
      const y = cy + (nS - 1 - (nS - 1) / 2) * 64;
      nodes.push({ id: 'more', kind: 'more', col: 'sess', x: X.sess, y: y - 20, w: 220, h: 40, color: 'var(--muted)', data: more });
    }
    // 没有会话的已启用渠道、没有任务会话的定时任务直接连到 Agent，表示“接上了”
    const agentOfChannel = (name: string) => botOf(name)?.agent ?? 'default';
    for (const c of chans) {
      if (c.status.state !== 'disabled' && !shown.some((s) => sessionChannels(s).includes(c.name))) {
        edges.push({ id: `c:${c.name}>ag:${agentOfChannel(c.name)}`, from: 'c:' + c.name, to: 'ag:' + agentOfChannel(c.name),
          color: channelMeta[c.name]?.color ?? 'var(--muted)', dashed: true });
      }
    }
    if (!shown.some((s) => s.sid.startsWith('cron:'))) {
      edges.push({ id: 'sched>ag:default', from: 'sched', to: 'ag:default', color: sc?.enabled ? 'var(--c-cron)' : 'var(--muted)', dashed: true });
    }

    // Agent 列：每个 agent 一个节点（各有自己的名字、人格、模型、绑定的机器人），
    // 下面是“Agent 默认值”（全局设置的家）和“添加 agent”
    const agents = ov.agents ?? [];
    const aH = 72;
    const aTop = cy - (agents.length * aH) / 2;
    agents.forEach((a: AgentInfo, i: number) => {
      nodes.push({ id: 'ag:' + a.id, kind: 'agent', col: 'agent', x: X.agent, y: aTop + i * aH, w: 210, h: 66, color: 'var(--c-agent)',
        target: { type: 'agent', id: a.id }, data: a });
      edges.push({ id: 'ag:' + a.id + '>gw', from: 'ag:' + a.id, to: 'gw', color: 'var(--c-agent)', flow: a.running });
    });
    const below = aTop + Math.max(agents.length, 1) * aH + 6;
    nodes.push({ id: 'agdefault', kind: 'agentdefaults', col: 'agent', x: X.agent, y: below, w: 210, h: 46, color: 'var(--muted)',
      target: { type: 'agentDefaults' }, data: null });
    nodes.push({ id: 'add:agent', kind: 'add', col: 'agent', x: X.agent, y: below + 54, w: 210, h: 44, color: 'var(--c-agent)',
      target: { type: 'agents' }, data: { icon: 'plus', title: '添加 agent', sub: '多一个一直存在的它' } });
    // 网关内部的群（房间）：每个群一个节点，连着它的成员 agent
    const roomsList = ov.rooms ?? [];
    const rTop = aTop - roomsList.length * 54 - 8;
    roomsList.forEach((r: RoomInfo, i: number) => {
      nodes.push({ id: 'rm:' + r.id, kind: 'room', col: 'agent', x: X.agent, y: rTop + i * 54, w: 210, h: 46, color: 'var(--accent-2)',
        target: { type: 'room', id: r.id }, data: r });
      for (const m of r.members) {
        if (agents.some((a) => a.id === m)) {
          edges.push({ id: 'rm:' + r.id + '>ag:' + m, from: 'rm:' + r.id, to: 'ag:' + m, color: 'var(--accent-2)', dashed: r.paused });
        }
      }
    });
    nodes.push({ id: 'add:room', kind: 'add', col: 'agent', x: X.agent, y: below + 106, w: 210, h: 44, color: 'var(--accent-2)',
      target: { type: 'rooms' }, data: { icon: 'chat', title: '创建群', sub: '几个 agent 一起聊' } });
    nodes.push({ id: 'gw', kind: 'gateway', col: 'gw', x: X.gw - 62, y: cy - 62, w: 124, h: 124, color: 'var(--c-gateway)', target: { type: 'gateway' } });

    // 能力：网关用的服务，从上到下是记忆、搜索、技能、模型（模型离中间最近，光点最多）
    nodes.push({ id: 'memory', kind: 'memory', col: 'caps', x: X.right, y: cy - 380, w: 250, h: 72, color: 'var(--c-memory)', target: { type: 'memory' }, data: ov.memory });
    edges.push({ id: 'gw>memory', from: 'gw', to: 'memory', color: 'var(--c-memory)' });
    const si = ov.search;
    const searchOn = !!si?.enabled && !!si?.key;
    nodes.push({ id: 'search', kind: 'search', col: 'caps', x: X.right, y: cy - 296, w: 250, h: 72, color: searchOn ? 'var(--c-search)' : 'var(--muted)', target: { type: 'search' }, data: si });
    edges.push({ id: 'gw>search', from: 'gw', to: 'search', color: searchOn ? 'var(--c-search)' : 'var(--muted)', dashed: !searchOn });
    nodes.push({ id: 'skills', kind: 'skills', col: 'caps', x: X.right, y: cy - 212, w: 250, h: 72, color: 'var(--c-skill)', target: { type: 'skills' }, data: ov.skills });
    edges.push({ id: 'gw>skills', from: 'gw', to: 'skills', color: 'var(--c-skill)' });
    nodes.push({ id: 'model', kind: 'model', col: 'caps', x: X.right, y: cy - 124, w: 250, h: 80, color: 'var(--c-model)', target: { type: 'model' } });
    edges.push({ id: 'gw>model', from: 'gw', to: 'model', color: 'var(--c-model)' });

    // 内核：跑对话循环的程序，各自在某台机器上（网关启动的本机内核排在最前面）
    const top = cy + 38;
    kernels.forEach((k, i) => {
      const id = 'k:' + k.node;
      nodes.push({ id, kind: 'kernel', col: 'caps', x: X.right, y: top + i * 86, w: 250, h: 70, color: 'var(--c-kernel)',
        target: { type: 'kernel', node: k.node }, data: k });
      edges.push({ id: `gw>${id}`, from: 'gw', to: id, color: 'var(--c-kernel)', flow: busyKernels.has(k.node) });
    });
    if (!kernels.length) {
      nodes.push({ id: 'k:none', kind: 'kernel', col: 'caps', x: X.right, y: top, w: 250, h: 70, color: 'var(--muted)', target: { type: 'kernel' }, data: null });
      edges.push({ id: 'gw>k:none', from: 'gw', to: 'k:none', color: 'var(--err)', dashed: true });
    }

    // 机器：本机、装了内核的电脑、SSH 服务器，最后是“接入机器”。内核连到它所在的机器；SSH 服务器由网关直接连
    const kidx = (m: MachineInfo) => {
      const i = kernels.findIndex((k) => (k.local ? '本机' : k.node) === m.name);
      return i < 0 ? kernels.length + (m.kind === 'ssh' ? 0 : 1) : i;
    };
    const machines: MachineInfo[] = store.machines.slice().sort((a, b) => kidx(a) - kidx(b));
    const mtop = top + Math.max(0, (Math.max(1, kernels.length) * 86 - nM * 96) / 2);
    machines.forEach((m, i) => {
      const id = 'm:' + m.name;
      nodes.push({ id, kind: 'machine', col: 'mach', x: X.mach, y: mtop + i * 96, w: 250, h: 80, color: m.online ? 'var(--c-kernel)' : 'var(--muted)',
        target: { type: 'machine', name: m.name }, data: m });
      const ks = kernels.filter((k) => (k.local ? '本机' : k.node) === m.name);
      for (const k of ks) edges.push({ id: `k:${k.node}>${id}`, from: 'k:' + k.node, to: id, color: 'var(--c-kernel)' });
      if (m.kind === 'ssh' || (m.kind === 'local' && !ks.length)) {
        // 网关自己执行：SSH 服务器，或者本机没有内核时
        edges.push({ id: `gw>${id}`, from: 'gw', to: id, color: m.online ? 'var(--accent-2)' : 'var(--err)', dashed: !m.online });
      }
    });
    const pairing = store.devices?.pairing?.length ?? 0;
    nodes.push({ id: 'add:machine', kind: 'add', col: 'mach', x: X.mach, y: mtop + machines.length * 96 + 8, w: 250, h: 60, color: pairing ? 'var(--accent)' : 'var(--c-kernel)',
      target: { type: 'machines' }, data: { icon: 'plus', title: pairing ? `${pairing} 台电脑请求接入` : '接入机器', sub: pairing ? '点开核对确认码' : '别的电脑、SSH 服务器' } });

    const order = (h: string) => machines.findIndex((m) => m.name === h);
    const list = (ov.workspaces ?? []).filter((w) => order(w.host) >= 0).sort((a, b) => order(a.host) - order(b.host)).slice(0, 10);
    const mcy = mtop + (machines.length * 96 - 16) / 2;
    list.forEach((w, i) => {
      const y = mcy + (i - (list.length - 1) / 2) * 56;
      nodes.push({ id: 'w:' + w.id, kind: 'workspace', col: 'ws', x: X.ws, y: y - 22, w: 210, h: 44, color: 'var(--c-dir)',
        target: { type: 'workspace', id: w.id }, data: w });
      edges.push({ id: `m:${w.host}>w:${w.id}`, from: 'm:' + w.host, to: 'w:' + w.id, color: 'var(--c-dir)', dashed: !w.online });
    });
    const wsBottom = list.length ? mcy + ((list.length - 1) / 2) * 56 + 22 : mcy - 22;
    nodes.push({ id: 'add:ws', kind: 'add', col: 'ws', x: X.ws, y: wsBottom + (list.length ? 14 : 0), w: 210, h: 44, color: 'var(--c-dir)',
      target: { type: 'workspace' }, data: { icon: 'plus', title: list.length ? '添加工作目录' : '还没有工作目录', sub: list.length ? '' : '点击添加一个' } });

    for (const n of nodes) {
      const o = offsets[n.id];
      if (o) {
        n.x += o.x;
        n.y += o.y;
      }
    }
    // 列名放在每一列最上面那个节点的上方
    const heads = COLS.map((c) => {
      const ys = nodes.filter((n) => n.col === c.col).map((n) => n.y);
      return { ...c, y: (ys.length ? Math.min(...ys) : cy) - 26 };
    });
    const byId = new Map(nodes.map((n) => [n.id, n]));
    return { nodes, edges, byId, heads, W: X.ws + 210, H, shownSids: new Set(shown.map((s) => s.sid)) };
  });

  // ---- 节点上的标记 ----

  const pendingRestart = $derived(store.fields.filter((f) => f.pending).length);
  function issueOf(n: N): Issue | null {
    const ov = store.ov!;
    switch (n.kind) {
      case 'channel':
        return n.data.status.state === 'error' ? { level: 'err', text: n.data.status.error || '出错了' } : null;
      case 'search':
        return n.data?.enabled && !n.data?.key ? { level: 'warn', text: '开着但没有可用的 key' } : null;
      case 'model':
        return ov.models?.models?.length ? null : { level: 'warn', text: '还没有模型：点开添加接口和模型' };
      case 'kernel':
        return n.data ? null : { level: 'err', text: '没有内核在线' };
      case 'machine':
        return n.data.online ? null : { level: 'err', text: n.data.detail || '现在用不了' };
      case 'schedule':
        return n.data?.error ? { level: 'err', text: n.data.error } : null;
      case 'gateway':
        if (pendingRestart) return { level: 'warn', text: `${pendingRestart} 项改动要重启网关才生效`, n: pendingRestart };
        if (ov.runtime?.update?.available) return { level: 'info', text: `有新版本 ${ov.runtime.update.latest}` };
        return null;
      case 'add':
        if (n.id === 'add:machine' && store.devices?.pairing?.length) return { level: 'warn', text: '有电脑在等你确认', n: store.devices.pairing.length };
        return null;
    }
    return null;
  }

  function anchor(n: N, side: 'l' | 'r') {
    return { x: n.x + (side === 'l' ? 0 : n.w), y: n.y + n.h / 2 };
  }

  function edgePath(e: E) {
    const a = layout.byId.get(e.from);
    const b = layout.byId.get(e.to);
    if (!a || !b) return '';
    const p = anchor(a, 'r');
    const q = anchor(b, 'l');
    const dx = Math.max(40, (q.x - p.x) * 0.5);
    return `M${p.x} ${p.y}C${p.x + dx} ${p.y} ${q.x - dx} ${q.y} ${q.x} ${q.y}`;
  }

  // ---- 平移和缩放 ----

  let el: HTMLDivElement;
  let view = $state({ x: 0, y: 0, k: 1 });
  let userMoved = false;
  let fitted = false; // 只在第一次拿到布局时自动适配，之后打开面板/加节点都不动视图
  let hover = $state<string | null>(null);

  function fit() {
    if (!el) return;
    const r = el.getBoundingClientRect();
    const pad = 48;
    const k = Math.min((r.width - pad * 2) / layout.W, (r.height - pad * 2) / layout.H, 1.15);
    view = { k, x: (r.width - layout.W * k) / 2, y: (r.height - layout.H * k) / 2 };
    userMoved = false;
    fitted = true;
  }

  $effect(() => {
    layout.W;
    layout.H;
    if (fitted || userMoved) return;
    queueMicrotask(() => {
      if (!fitted) fit();
    });
  });

  function onWheel(e: WheelEvent) {
    e.preventDefault();
    const r = el.getBoundingClientRect();
    const mx = e.clientX - r.left;
    const my = e.clientY - r.top;
    const k = Math.min(2.5, Math.max(0.3, view.k * Math.exp(-e.deltaY * 0.0015)));
    view = { k, x: mx - (mx - view.x) * (k / view.k), y: my - (my - view.y) * (k / view.k) };
    userMoved = true;
  }

  let drag: { x: number; y: number; vx: number; vy: number } | null = null;
  let nodeDrag: { id: string; x: number; y: number; ox: number; oy: number; moved: boolean } | null = null;
  let dragged = $state<string | null>(null);
  let swallowClick = false;

  function onDown(e: PointerEvent) {
    if (e.button !== 0) return;
    const g = (e.target as Element).closest('.node') as SVGGElement | null;
    swallowClick = false;
    if (g) {
      // 拖节点时不捕获指针：否则松手时的点击事件落到画布上，节点就点不开了
      const id = g.dataset.id!;
      const o = offsets[id] ?? { x: 0, y: 0 };
      nodeDrag = { id, x: e.clientX, y: e.clientY, ox: o.x, oy: o.y, moved: false };
    } else if (!isChrome(e)) {
      drag = { x: e.clientX, y: e.clientY, vx: view.x, vy: view.y };
      el.setPointerCapture(e.pointerId);
    }
  }
  function onMove(e: PointerEvent) {
    if (nodeDrag) {
      const dx = e.clientX - nodeDrag.x;
      const dy = e.clientY - nodeDrag.y;
      if (!nodeDrag.moved && Math.hypot(dx, dy) < 4) return;
      nodeDrag.moved = true;
      dragged = nodeDrag.id;
      offsets[nodeDrag.id] = { x: nodeDrag.ox + dx / view.k, y: nodeDrag.oy + dy / view.k };
      return;
    }
    if (!drag) return;
    view = { ...view, x: drag.vx + e.clientX - drag.x, y: drag.vy + e.clientY - drag.y };
    userMoved = true;
  }
  function onUp() {
    if (nodeDrag?.moved) {
      saveOffsets();
      swallowClick = true; // 拖完松手不算点击
      userMoved = true;
    }
    nodeDrag = null;
    dragged = null;
    drag = null;
  }

  // ---- 光点：数据沿着连线流动 ----

  type Pulse = { id: number; edge: string; color: string; start: number; dur: number; reverse: boolean; x: number; y: number; r: number };
  let pulses = $state<Pulse[]>([]);
  const paths: Record<string, SVGPathElement> = {};
  let seq = 0;
  let raf = 0;

  function reg(node: SVGPathElement, id: string) {
    paths[id] = node;
    return {
      update(nid: string) {
        paths[nid] = node;
      },
      destroy() {
        if (paths[id] === node) delete paths[id];
      },
    };
  }

  function pulse(edge: string, color: string, opts: { reverse?: boolean; delay?: number; dur?: number; r?: number } = {}) {
    if (!layout.edges.some((e) => e.id === edge)) return;
    const start = performance.now() + (opts.delay ?? 0);
    pulses.push({ id: ++seq, edge, color, start, dur: opts.dur ?? 900, reverse: !!opts.reverse, x: -999, y: -999, r: opts.r ?? 3.6 });
    if (!raf) raf = requestAnimationFrame(tick);
  }

  function chain(steps: [string, boolean][], color: string, dur = 700) {
    steps.forEach(([edge, reverse], i) => pulse(edge, color, { reverse, delay: i * dur * 0.85, dur }));
  }

  function tick(now: number) {
    raf = 0;
    const keep: Pulse[] = [];
    for (const p of pulses) {
      const path = paths[p.edge];
      if (!path) continue;
      const t = (now - p.start) / p.dur;
      if (t > 1) continue;
      if (t >= 0) {
        const e = t < 0.5 ? 2 * t * t : 1 - Math.pow(-2 * t + 2, 2) / 2;
        const len = path.getTotalLength();
        const pt = path.getPointAtLength((p.reverse ? 1 - e : e) * len);
        p.x = pt.x;
        p.y = pt.y;
      }
      keep.push(p);
    }
    pulses = keep;
    if (keep.length) raf = requestAnimationFrame(tick);
  }

  const busyModel = new Map<string, number>();
  const lastDelta = new Map<string, number>();

  // 消息从入口进来：入口 → 会话 → Agent → 网关（会话不在图上时：入口 → Agent → 网关）
  function inbound(sid: string, ch: string): [string, boolean][] {
    if (layout.shownSids.has(sid)) return [[`c:${ch}>s:${sid}`, false], [`s:${sid}>agent`, false], ['agent>gw', false]];
    return [[`c:${ch}>agent`, false], ['agent>gw', false]];
  }

  // 每个会话正在处理的这一轮是从哪个渠道来的：回复的光点流回那里
  const origin = new Map<string, string>();

  // 工具调用参数里的 workspace 指的是哪个挂载的工作目录（只挂了一个时可以不写）
  function mountOf(s: SessionInfo, raw: unknown): MountInfo | undefined {
    let want = '';
    try {
      const a = typeof raw === 'string' ? JSON.parse(raw) : raw;
      want = String((a as any)?.workspace ?? '');
    } catch {
      /* 参数不完整 */
    }
    if (!want) return s.workspaces.length === 1 ? s.workspaces[0] : undefined;
    return s.workspaces.find((m, i) => `${m.host}:${m.path}` === want || m.host === want || m.path === want || String(i + 1) === want);
  }
  function toolPath(s: SessionInfo, mt: MountInfo): string[] {
    const m = 'm:' + mt.host;
    const w = `${m}>w:${mt.id}`;
    const k = s.run_node || s.kernel;
    if (mt.how === 'local' && k) return [`gw>k:${k}`, `k:${k}>${m}`, w];
    if (layout.edges.some((x) => x.id === `gw>${m}`)) return [`gw>${m}`, w];
    const other = (store.ov?.kernels ?? []).find((x) => (x.local ? '本机' : x.node) === mt.host);
    return other ? [`gw>k:${other.node}`, `k:${other.node}>${m}`, w] : [w];
  }
  const lastPath = new Map<string, string>();

  function onEvent(e: Ev) {
    const sid = e.sid ?? '';
    const s = store.session(sid);
    const ch = (e.t === 'message' ? e.data?.channel : origin.get(sid)) ?? s?.channel ?? 'web';
    const chColor = channelMeta[ch]?.color ?? 'var(--c-session)';
    const back = (): [string, boolean][] => inbound(sid, ch).map(([x]) => [x, true] as [string, boolean]).reverse();
    switch (e.t) {
      case 'message':
        chain(inbound(sid, ch), chColor);
        break;
      case 'model':
        if (e.kind === 'start') {
          pulse('gw>model', 'var(--c-model)', { dur: 650 });
          clearInterval(busyModel.get(sid));
          busyModel.set(sid, window.setInterval(() => pulse('gw>model', 'var(--c-model)', { dur: 650, r: 2.6 }), 700));
        } else {
          clearInterval(busyModel.get(sid));
          busyModel.delete(sid);
        }
        break;
      case 'delta': {
        const now = performance.now();
        if (now - (lastDelta.get(sid) ?? 0) < 260) break;
        lastDelta.set(sid, now);
        clearInterval(busyModel.get(sid));
        busyModel.delete(sid);
        const color = e.kind === 'reasoning' ? 'var(--c-model)' : 'var(--accent-2)';
        pulse('gw>model', color, { reverse: true, dur: 600, r: 3 });
        pulse('agent>gw', color, { reverse: true, delay: 380, dur: 600, r: 3 });
        if (layout.shownSids.has(sid)) pulse(`s:${sid}>agent`, color, { reverse: true, delay: 760, dur: 600, r: 3 });
        break;
      }
      case 'tool': {
        if (e.name === 'WebSearch' || e.name === 'WebFetch') {
          // 网络搜索由网关自己调 DeepSeek
          lastPath.set(sid, 'gw>search');
          pulse('gw>search', 'var(--c-search)', { dur: 600 });
          break;
        }
        if (e.name === 'Skill') {
          lastPath.set(sid, 'gw>skills');
          pulse('gw>skills', 'var(--c-skill)', { dur: 600 });
          break;
        }
        // 工具在工作目录所在的机器上执行：沿着 网关 → 内核 → 机器 → 工作目录（或者 网关 → 机器）流过去
        const mt = s ? mountOf(s, e.data) : undefined;
        if (!s || !mt) break;
        const steps = toolPath(s, mt);
        lastPath.set(sid, steps[0]);
        chain(steps.map((x) => [x, false]), 'var(--c-kernel)', 520);
        break;
      }
      case 'tool_result': {
        const first = lastPath.get(sid);
        if (first) pulse(first, e.ok ? 'var(--c-kernel)' : 'var(--err)', { reverse: true, dur: 600 });
        break;
      }
      case 'say':
        chain(back(), chColor);
        break;
      case 'context':
        if (e.kind === 'start') pulse('gw>memory', 'var(--c-memory)', { dur: 700 });
        break;
      case 'run':
        if (e.kind === 'start' && e.data?.channel) origin.set(sid, e.data.channel);
        if (e.kind === 'end') {
          clearInterval(busyModel.get(sid));
          busyModel.delete(sid);
          chain(back(), 'var(--accent-2)');
        }
        break;
    }
  }

  // 画布窄的时候（右边开着面板、窗口小），图例只留颜色，不放操作说明
  let narrow = $state(false);
  onMount(() => {
    const off = store.on(onEvent);
    const ro = new ResizeObserver(() => (narrow = el.clientWidth < 1100));
    ro.observe(el);
    return () => {
      off();
      ro.disconnect();
      cancelAnimationFrame(raf);
      for (const t of busyModel.values()) clearInterval(t);
    };
  });

  // ---- 状态 ----

  const phases = $derived(Object.values(store.phase));
  const modelBusy = $derived(phases.includes('thinking') || phases.includes('writing'));
  // 正在执行工具的会话挂着的机器
  const busyHosts = $derived(
    new Set((store.ov?.sessions ?? []).filter((s) => store.phase[s.sid] === 'tool').flatMap((s) => s.workspaces.map((m) => m.host))),
  );
  const t = $derived(store.ov!.totals);
  const ov = $derived(store.ov!);

  function hot(e: E) {
    return hover !== null && (e.from === hover || e.to === hover);
  }

  function pctOf(key: string, def: number) {
    const v = Number(store.field(key)?.value);
    return Math.round((v > 0 ? v : def) * 100) + '%';
  }
  function hostOf(u: string) {
    try {
      return new URL(u).host;
    } catch {
      return u;
    }
  }
  function agentSub(a: AgentInfo): string {
    const bits = [`${a.sessions} 个会话`];
    if (a.bots.length) bits.push(`${a.bots.length} 个机器人`);
    return bits.join(' · ');
  }

  function machineSub(m: MachineInfo) {
    if (!m.online) return truncate(m.detail || '现在用不了', 18);
    const box = m.landlock > 0 ? `Landlock v${m.landlock} 沙箱` : '没有沙箱';
    const os = osLabel(m.os);
    if (m.kind === 'ssh') return [os, '网关直连', box].filter(Boolean).join(' · ');
    if (m.kernels.length) return truncate([os, `内核 ${m.kernels.join('、')}`, box].filter(Boolean).join(' · '), 26);
    return [os, '网关执行工具', box].filter(Boolean).join(' · ');
  }

  function sessSub(s: SessionInfo) {
    const ph = store.phase[s.sid];
    if (ph === 'thinking') return '正在思考…';
    if (ph === 'writing') return '正在回复…';
    if (ph === 'tool') return '正在执行工具…';
    if (s.running) return '处理中…';
    const mode = s.workspaces.length ? (modeMeta[s.mode]?.label ?? s.mode) + (s.workspaces.length > 1 ? ` · ${s.workspaces.length} 个目录` : '') : '未挂目录';
    return `${mode} · ${s.last_at ? ago(s.last_at * 1000) : '新会话'}`;
  }

  // ---- 右键菜单：常用操作（和面板里的操作一样） ----

  let menu = $state<{ x: number; y: number; title: string; items: MenuItem[] } | null>(null);
  let mountFor = $state<SessionInfo | null>(null); // 给这个会话挂载工作目录
  let browseOn = $state<MachineInfo | null>(null); // 浏览这台机器，添加工作目录

  async function act(fn: () => Promise<unknown>, ok?: string) {
    try {
      await fn();
      if (ok) store.toast({ kind: 'ok', text: ok }, 2000);
    } catch (err) {
      store.fail(err);
    }
  }

  async function sshAction(name: string, op: 'connect' | 'disconnect') {
    const d = store.devices ?? (await store.loadDevices());
    const c = d?.connections.find((x) => x.name === name);
    if (!c) throw new Error('没有找到这台服务器的连接');
    await api.post(`/api/devices/${c.id}/${op}`);
    store.loadDevices();
  }

  function menuFor(n: N): { title: string; items: MenuItem[] } | null {
    const open: MenuItem = { label: '查看和设置', icon: 'sliders', run: () => n.target && store.open(n.target) };
    switch (n.kind) {
      case 'session': {
        const s = n.data as SessionInfo;
        return { title: '会话 · ' + s.title, items: [
          { label: '打开对话', icon: 'chat', run: () => (location.hash = '#/sessions/' + encodeURIComponent(s.sid)) },
          open,
          { label: '挂载 / 取下工作目录…', icon: 'folder', run: () => (mountFor = s) },
          'sep',
          { label: '停止当前任务', icon: 'stop', disabled: !s.running, run: () => act(() => api.post(`${sidPath(s.sid)}/stop`), '已停止') },
          { label: '归档并重置上下文', icon: 'undo', run: () => confirm(`归档「${s.title}」的历史并清空上下文？`) && act(() => api.post(`${sidPath(s.sid)}/reset`), '上下文已清空') },
          ...(s.sid === 'main' ? [] : [{ label: '删除会话', icon: 'trash', danger: true,
            run: () => confirm(`删除会话「${s.title}」？历史和设置都会删掉。`) && act(() => api.del(sidPath(s.sid)), '会话已删除') } as MenuItem]),
        ] };
      }
      case 'channel': {
        const c = n.data;
        const on = c.status.state !== 'disabled';
        // 另外加的机器人：开关和凭据在它自己的节点里
        const toggle: MenuItem[] = c.name === 'web' || n.target?.type === 'bot' ? [] : [{ label: on ? '停用' : '启用', icon: on ? 'stop' : 'zap',
          run: () => store.setConfig({ [`${c.name}.enabled`]: !on }) }];
        return { title: c.meta.label, items: [open, ...toggle] };
      }
      case 'schedule':
        return { title: '定时任务', items: [open, { label: '新建任务…', icon: 'plus', run: () => { store.scheduleNew = true; store.open({ type: 'schedules' }); } }] };
      case 'room': {
        const rm = n.data as RoomInfo;
        return { title: '群 · ' + rm.name, items: [
          open,
          { label: '打开聊天', icon: 'chat', run: () => (location.hash = '#/sessions/' + encodeURIComponent(rm.sid)) },
          { label: rm.paused ? '恢复' : '暂停', icon: rm.paused ? 'play' : 'stop',
            run: () => act(() => api.patch('/api/rooms/' + encodeURIComponent(rm.id), { paused: !rm.paused }), rm.paused ? '已恢复' : '已暂停') },
        ] };
      }
      case 'agent': {
        const ag = n.data as AgentInfo;
        return { title: 'Agent · ' + ag.name, items: [
          open,
          { label: '编辑人格…', icon: 'pen', run: () => (store.persona = { name: 'persona.md', isNew: false, agent: ag.id }) },
          { label: '打开它的会话', icon: 'chat', run: () => (location.hash = '#/sessions/' + encodeURIComponent(ag.default ? 'main' : '@' + ag.id + '/main')) },
          { label: '加一个机器人…', icon: 'plus', run: () => store.open({ type: 'bots' }) },
        ] };
      }
      case 'gateway':
        return { title: '网关', items: [
          open,
          { label: '检查更新', icon: 'refresh', run: () => store.checkUpdate() },
          { label: '重启网关', icon: 'refresh', run: () => confirm('重启网关？正在处理的消息会中断，页面会自动重连。') && store.restartGateway() },
        ] };
      case 'kernel': {
        const k = n.data as KernelInfo | null;
        if (!k) return { title: '内核', items: [open, { label: '接入电脑…', icon: 'laptop', run: () => store.open({ type: 'machines' }) }] };
        const paired = !k.local;
        return { title: '内核 · ' + k.node, items: [
          open,
          { label: '它所在的机器', icon: 'hub', run: () => store.open({ type: 'machine', name: k.local ? '本机' : k.node }) },
          ...(paired ? ['sep', { label: '移除这台电脑（口令作废）', icon: 'trash', danger: true,
            run: () => confirm(`移除 ${k.node}？它会断开，要再接入需重新配对。`) && act(() => api.del(`/api/paired/${encodeURIComponent(k.node)}`), '已移除') }] as MenuItem[] : []),
        ] };
      }
      case 'machine': {
        const m = n.data as MachineInfo;
        const items: MenuItem[] = [
          open,
          { label: '浏览目录，添加工作目录…', icon: 'folder', disabled: !m.online, run: () => (browseOn = m) },
        ];
        if (m.kind === 'ssh') {
          items.push('sep',
            m.online
              ? { label: '断开 SSH 连接', icon: 'stop', run: () => act(() => sshAction(m.name, 'disconnect'), '正在断开') }
              : { label: '连接', icon: 'link', run: () => act(() => sshAction(m.name, 'connect'), '正在连接') });
        }
        return { title: (m.kind === 'local' ? '本机 · 网关所在的机器' : machineMeta[m.kind].label + ' · ' + m.name), items };
      }
      case 'workspace': {
        const w = n.data as WorkspaceInfo;
        const main = store.session('main');
        const mounted = main?.workspaces.some((x) => x.id === w.id);
        return { title: `${w.host}:${w.path}`, items: [
          open,
          ...(main ? [{ label: mounted ? '从主会话取下' : '挂到主会话', icon: 'hub',
            run: () => act(async () => store.upsertSession(await api.patch<SessionInfo>(sidPath('main'),
              { workspaces: mounted ? main.workspaces.filter((x) => x.id !== w.id).map((x) => x.id) : [...main.workspaces.map((x) => x.id), w.id] })),
              mounted ? '已从主会话取下' : '已挂到主会话') } as MenuItem] : []),
          { label: w.default ? '新会话不再默认挂它' : '新会话默认挂上它', icon: 'check',
            run: () => act(() => api.put('/api/workspaces/default', { id: w.id, default: !w.default })) },
          'sep',
          { label: '从列表里删掉', icon: 'trash', danger: true,
            run: () => confirm(`从列表里删掉 ${w.host}:${w.path}？机器上的文件不会动。`) && act(() => api.del(`/api/workspaces/${w.id}`), '已删掉') },
        ] };
      }
      case 'model': {
        const dm = ov.models?.models?.find((m) => m.default);
        return { title: '模型', items: [
          open,
          { label: '添加模型', icon: 'plus', run: () => store.openModel('model') },
          { label: '添加接口', icon: 'plus', run: () => store.openModel('endpoint') },
          { label: '测试默认模型', icon: 'zap', disabled: !dm,
            run: () => act(async () => {
              const r = await api.post<{ ok: boolean; error?: string; ms?: number }>('/api/test/model', { id: '' });
              if (!r.ok) throw new Error(r.error || '没通');
              store.toast({ kind: 'ok', text: `模型连接正常 · ${r.ms ?? '—'} ms` }, 2200);
            }) },
        ] };
      }
      case 'memory':
      case 'search':
      case 'skills':
      case 'add':
      case 'agentdefaults':
      case 'botadd':
        return n.target ? { title: '', items: [open] } : null;
      case 'more':
        return { title: '会话', items: [{ label: '查看全部会话', icon: 'chat', run: () => (location.hash = '#/sessions') }] };
    }
    return null;
  }

  function openMenu(ev: MouseEvent, n: N) {
    ev.preventDefault();
    ev.stopPropagation();
    const m = menuFor(n);
    if (m) menu = { x: ev.clientX, y: ev.clientY, ...m };
  }

  async function addOn(path: string) {
    const m = browseOn!;
    browseOn = null;
    await act(async () => {
      const w = await api.post<WorkspaceInfo>('/api/workspaces', { host: m.name, path });
      store.open({ type: 'workspace', id: w.id });
    }, `已添加 ${m.name}:${path}`);
  }

  // 画布上的浮层（按钮、统计、提示、图例）不是空白，别把它们的右键/双击当成画布操作
  function isChrome(e: Event) {
    return !!(e.target as Element).closest('.node, button, .hint, .legend, .tools, .stats');
  }

  // 右键画布空白：添加东西。节点自己的右键菜单会 stopPropagation，不会走到这里。
  function openBlankMenu(ev: MouseEvent) {
    ev.preventDefault();
    if (isChrome(ev)) return;
    menu = { x: ev.clientX, y: ev.clientY, title: '画布', items: [
      { label: '接入电脑 / SSH 服务器…', icon: 'laptop', run: () => store.open({ type: 'machines' }) },
      { label: '添加工作目录…', icon: 'folder', run: () => store.open({ type: 'workspace' }) },
      { label: '新增模型接口…', icon: 'plus', run: () => store.openModel('endpoint') },
      { label: '新建定时任务…', icon: 'clock', run: () => { store.scheduleNew = true; store.open({ type: 'schedules' }); } },
      { label: '搜索设置…', icon: 'search', run: () => (store.palette = true) },
      'sep',
      { label: '适应窗口', icon: 'fit', run: () => fit() },
    ] };
  }

  function activate(n: N) {
    if (swallowClick) {
      swallowClick = false;
      return;
    }
    if (n.kind === 'more') location.hash = '#/sessions';
    else if (n.target) store.open(n.target);
  }

  const selectedId = $derived.by(() => {
    const t = store.inspector;
    if (!t) return null;
    switch (t.type) {
      case 'channel': return 'c:' + t.name;
      case 'session': return 's:' + t.sid;
      case 'schedules': return 'sched';
      case 'agent': return 'agent';
      case 'gateway': return 'gw';
      case 'model': case 'search': case 'memory': case 'skills': return t.type;
      case 'kernel': return t.node ? 'k:' + t.node : 'k:none';
      case 'machine': return 'm:' + t.name;
      case 'machines': return 'add:machine';
      case 'workspace': return t.id ? 'w:' + t.id : 'add:ws';
    }
    return null;
  });
</script>

<div
  class="canvas"
  bind:this={el}
  onwheel={onWheel}
  onpointerdown={onDown}
  onpointermove={onMove}
  onpointerup={onUp}
  onpointercancel={onUp}
  onpointerleave={onUp}
  ondblclick={(e) => !isChrome(e) && fit()}
  oncontextmenu={openBlankMenu}
  role="application"
  aria-label="网关拓扑图"
>
  <svg width="100%" height="100%">
    <defs>
      <pattern id="dots" width="22" height="22" patternUnits="userSpaceOnUse">
        <circle cx="1" cy="1" r="1" class="dotgrid" />
      </pattern>
      <filter id="glow" x="-50%" y="-50%" width="200%" height="200%">
        <feGaussianBlur stdDeviation="3" result="b" />
        <feMerge><feMergeNode in="b" /><feMergeNode in="SourceGraphic" /></feMerge>
      </filter>
      <filter id="soft" x="-30%" y="-30%" width="160%" height="160%">
        <feGaussianBlur stdDeviation="10" />
      </filter>
      <radialGradient id="core" cx="45%" cy="40%" r="70%">
        <stop offset="0%" stop-color="var(--accent-2)" />
        <stop offset="70%" stop-color="var(--accent)" />
        <stop offset="100%" stop-color="color-mix(in srgb, var(--accent) 70%, #000)" />
      </radialGradient>
    </defs>
    <rect width="100%" height="100%" fill="url(#dots)" />

    <g transform="translate({view.x} {view.y}) scale({view.k})">
      <!-- 列名 -->
      {#each layout.heads as h (h.col)}
        <text x={h.x + (h.col === 'gw' ? h.w / 2 : 2)} y={h.y} class="colhead" class:center={h.col === 'gw'}>{h.label}</text>
      {/each}

      <!-- 连线 -->
      {#each layout.edges as e (e.id)}
        <path
          use:reg={e.id}
          d={edgePath(e)}
          class="edge"
          class:hot={hot(e)}
          class:dim={hover !== null && !hot(e)}
          class:dashed={e.dashed}
          class:flow={e.flow}
          style="--c: {e.color}"
        />
      {/each}

      <!-- 光点 -->
      {#each pulses as p (p.id)}
        <circle cx={p.x} cy={p.y} r={p.r} fill={p.color} filter="url(#glow)" class="pulse" />
      {/each}

      <!-- 节点 -->
      {#each layout.nodes as n (n.id)}
        {@const issue = issueOf(n)}
        <g
          class="node {n.kind}"
          class:dragging={dragged === n.id}
          class:selected={selectedId === n.id}
          data-id={n.id}
          class:dimmed={hover !== null && hover !== n.id && !layout.edges.some((e) => (e.from === hover && e.to === n.id) || (e.to === hover && e.from === n.id))}
          transform="translate({n.x} {n.y})"
          style="--c: {n.color}"
          role="button"
          tabindex="0"
          onclick={() => activate(n)}
          oncontextmenu={(ev) => openMenu(ev, n)}
          ondblclick={(ev) => { ev.stopPropagation(); if (n.kind === 'session') location.hash = '#/sessions/' + encodeURIComponent(n.data.sid); }}
          onkeydown={(ev) => ev.key === 'Enter' && activate(n)}
          onmouseenter={() => (hover = n.id)}
          onmouseleave={() => (hover = null)}
        >
          {#if n.kind === 'channel'}
            {@const c = n.data}
            {@const off = c.status.state === 'disabled'}
            <rect class="card" class:off width={n.w} height={n.h} rx="16" />
            <rect class="stripe" x="0" y="16" width="3" height="32" rx="1.5" />
            <circle class="disc" cx="34" cy="32" r="18" />
            <g class="ico"><Icon name={c.meta.icon} x={23} y={21} size={22} /></g>
            <text x="62" y="28" class="t1">{c.meta.label}</text>
            <text x="62" y="47" class="t2" class:errt={c.status.state === 'error'}>
              {off ? '未启用 · 点击配置' : c.status.state === 'error' ? truncate(c.status.error || '出错', 14) : truncate(c.status.identity || stateLabel[c.status.state] || '', 16)}
            </text>
            <circle cx={n.w - 16} cy="16" r="4" class="sdot {c.status.state}" />
          {:else if n.kind === 'session'}
            {@const s = n.data as SessionInfo}
            {@const ph = store.phase[s.sid]}
            {#if s.running || s.pending}
              <rect class="ring {ph ?? ''}" class:pending={s.pending > 0} x="-4" y="-4" width={n.w + 8} height={n.h + 8} rx="28" />
            {/if}
            <rect class="card" width={n.w} height={n.h} rx="24" />
            <circle class="avatar" cx="24" cy="24" r="15" />
            {#if s.sid === 'main'}
              <g class="ico"><Icon name="hub" x={15} y={15} size={18} /></g>
            {:else}
              <text x="24" y="29" class="initial">{Array.from(s.title.replace(/^.*?·\s*/, ''))[0] ?? '?'}</text>
            {/if}
            <text x="48" y="21" class="t1">{truncate(s.title, 12)}</text>
            <text x="48" y="37" class="t2" class:live={!!ph}>{sessSub(s)}</text>
            {#if s.pending > 0}
              <circle cx={n.w - 18} cy="24" r="9" class="badge" />
              <text x={n.w - 18} y="28" class="badge-t">!</text>
            {/if}
          {:else if n.kind === 'more'}
            <rect class="card more" width={n.w} height={n.h} rx="20" />
            <text x={n.w / 2} y="25" class="t2 center">还有 {n.data} 个会话 →</text>
          {:else if n.kind === 'agent'}
            {@const ag = n.data as AgentInfo}
            <rect class="card agent-card" class:active={ag.running} width={n.w} height={n.h} rx="20" />
            <circle class="disc" cx="34" cy="33" r="21" />
            <text x="34" y="41" class="emoji">{ag.emoji || '🙂'}</text>
            <text x="66" y="26" class="t3">{ag.default ? 'AGENT · 默认' : 'AGENT'}</text>
            <text x="66" y="45" class="t1">{truncate(ag.name, 10)}</text>
            <text x="66" y="60" class="t2">{truncate(agentSub(ag), 22)}</text>
          {:else if n.kind === 'gateway'}
            <g transform="translate(62 62)">
              <circle r="92" class="halo" filter="url(#soft)" />
              <g class="orbit">
                <circle r="70" class="orbit-ring" />
                <circle cx="70" cy="0" r="4" class="sat a" />
                <circle cx="-49" cy="49" r="3" class="sat b" />
              </g>
              <circle r="56" class="core" />
              <circle r="56" class="core-edge" />
              <text y="-2" class="gw-title">nano</text>
              <text y="17" class="gw-sub">网关</text>
              <text y="94" class="gw-stat">{t.runs} 轮 · 缓存 {pct(t.cached_tokens, t.prompt_tokens)}</text>
            </g>
          {:else if n.kind === 'model'}
            {@const dm = ov.models?.models?.find((m) => m.default)}
            {@const de = ov.models?.endpoints?.find((e) => e.id === dm?.endpoint)}
            {@const mn = ov.models?.models?.length ?? 0}
            <rect class="card" class:active={modelBusy} class:off={!dm} width={n.w} height={n.h} rx="16" />
            <circle class="disc" cx="36" cy="40" r="20" />
            <g class="ico"><Icon name="sparkles" x={24} y={28} size={24} /></g>
            {#if dm}
              <text x="66" y="28" class="t3">模型 · {de?.kind ?? ''}{mn > 1 ? ` · ${mn} 个` : ''}</text>
              <text x="66" y="48" class="t1">{truncate(dm.label, 19)}</text>
              <text x="66" y="66" class="t2">{truncate(hostOf(de?.base_url || '') || '—', 16)} · 思考 {dm.thinking === 'none' ? '关闭' : dm.thinking || '默认'}</text>
            {:else}
              <text x="66" y="28" class="t3">模型 · 还没有配置</text>
              <text x="66" y="48" class="t1">点开添加接口和模型</text>
              <text x="66" y="66" class="t2">模型只来自模型列表</text>
            {/if}
          {:else if n.kind === 'search'}
            {@const si = n.data as SearchInfo | undefined}
            {@const on = !!si?.enabled && !!si?.key}
            <rect class="card" class:off={!on} width={n.w} height={n.h} rx="16" />
            <circle class="disc" cx="34" cy="36" r="18" />
            <g class="ico"><Icon name="search" x={23} y={25} size={22} /></g>
            <text x="62" y="24" class="t3">网络搜索 · DeepSeek</text>
            <text x="62" y="43" class="t1">{!si?.enabled ? '已关闭' : on ? '模型可以搜网页' : '没有可用的 key'}</text>
            <text x="62" y="60" class="t2" class:errt={si?.enabled && !on}>{on ? `今天搜索 ${si.today.search} 次 · 读网页 ${si.today.fetch} 次` : si?.enabled ? '点开填 key' : '点开打开'}</text>
          {:else if n.kind === 'memory'}
            {@const mi = n.data as MemoryInfo | undefined}
            <rect class="card" width={n.w} height={n.h} rx="16" />
            <circle class="disc" cx="34" cy="36" r="18" />
            <g class="ico"><Icon name="brain" x={23} y={25} size={22} /></g>
            <text x="62" y="24" class="t3">上下文与记忆</text>
            <text x="62" y="43" class="t1">到 {pctOf('context.compact_at', 0.75)} 自动整理到 {pctOf('context.compact_to', 0.4)}</text>
            <text x="62" y="60" class="t2">{mi?.summarized ?? 0} 个会话整理过 · {mi?.archives ?? 0} 份归档</text>
          {:else if n.kind === 'skills'}
            {@const list = n.data?.list ?? []}
            <rect class="card" width={n.w} height={n.h} rx="16" />
            <circle class="disc" cx="34" cy="36" r="18" />
            <g class="ico"><Icon name="book" x={23} y={25} size={22} /></g>
            <text x="62" y="24" class="t3">技能</text>
            <text x="62" y="43" class="t1">{list.length} 个技能</text>
            <text x="62" y="60" class="t2">{truncate(list.map((s: { name: string }) => s.name).join('、') || '还没有技能', 22)}</text>
          {:else if n.kind === 'schedule'}
            {@const sc = n.data as ScheduleSummary | undefined}
            <rect class="card" class:off={!sc?.total} width={n.w} height={n.h} rx="16" />
            <rect class="stripe" x="0" y="16" width="3" height="32" rx="1.5" />
            <circle class="disc" cx="34" cy="32" r="18" />
            <g class="ico"><Icon name="clock" x={23} y={21} size={22} /></g>
            <text x="62" y="28" class="t1">定时任务</text>
            <text x="62" y="47" class="t2" class:errt={!!sc?.error}>
              {sc?.error ? '读不了任务文件' : !sc?.total ? '还没有任务 · 点击新建' : sc.running ? `${sc.running} 个正在运行` : sc.enabled ? `${sc.enabled} 个开着 · 下次 ${whenText(sc.next)}` : `${sc.total} 个，都暂停了`}
            </text>
            {#if sc?.total}<circle cx={n.w - 16} cy="16" r="4" class="sdot {sc.running ? 'starting' : sc.enabled ? 'online' : 'disabled'}" />{/if}
          {:else if n.kind === 'kernel'}
            {@const k = n.data as KernelInfo | null}
            <rect class="card" class:active={!!k && busyKernels.has(k.node)} class:bad={!k} width={n.w} height={n.h} rx="16" />
            <circle class="disc" cx="34" cy="35" r="18" />
            <g class="ico"><Icon name="shield" x={23} y={24} size={22} /></g>
            {#if k}
              <text x="62" y="24" class="t3">{k.managed ? '内核 · 网关启动的' : k.local ? '内核 · 在本机' : '内核 · 在别的电脑上'}</text>
              <text x="62" y="43" class="t1">{truncate(k.node, 18)}</text>
              <text x="62" y="59" class="t2">{truncate([osLabel(k.os), k.version ? 'v' + k.version : '', k.landlock > 0 ? `Landlock v${k.landlock}` : '没有沙箱', busyKernels.has(k.node) ? '正在跑' : ''].filter(Boolean).join(' · '), 26)}</text>
              <circle cx={n.w - 16} cy="16" r="4" class="sdot online" />
            {:else}
              <text x="62" y="31" class="t1 errt">没有内核在线</text>
              <text x="62" y="49" class="t2">点开看怎么办</text>
            {/if}
          {:else if n.kind === 'machine'}
            {@const m = n.data as MachineInfo}
            <rect class="card" class:active={busyHosts.has(m.name)} class:off={!m.online} width={n.w} height={n.h} rx="16" />
            <circle class="disc" cx="36" cy="40" r="20" />
            <g class="ico"><Icon name={machineMeta[m.kind].icon} x={24} y={28} size={24} /></g>
            <text x="66" y="28" class="t3">{m.kind === 'local' ? '网关所在的机器' : machineMeta[m.kind].label}</text>
            <text x="66" y="48" class="t1">{truncate(m.name, 18)}</text>
            <text x="66" y="66" class="t2" class:errt={!m.online}>{machineSub(m)}</text>
            <circle cx={n.w - 16} cy="16" r="4" class="sdot {m.online ? 'online' : 'error'}" />
          {:else if n.kind === 'workspace'}
            {@const w = n.data as WorkspaceInfo}
            <rect class="card" width={n.w} height={n.h} rx="12" />
            <g class="ico small"><Icon name="folder" x={13} y={12} size={20} /></g>
            <text x="42" y="20" class="t1 mono">{shortPath(homeShort(w.path), 22)}</text>
            <text x="42" y="35" class="t2">{w.default ? '默认 · ' : ''}{w.sessions} 个会话</text>
          {:else if n.kind === 'room'}
            {@const rm = n.data as RoomInfo}
            <rect class="card" width={n.w} height={n.h} rx="14" />
            <circle class="disc" cx="26" cy="23" r="14" />
            <g class="ico"><Icon name="chat" x={18} y={15} size={16} /></g>
            <text x="48" y="21" class="t1">{truncate(rm.name, 12)}</text>
            <text x="48" y="36" class="t2">{rm.members.length} 个 agent{rm.paused ? ' · 已暂停' : ''} · {rm.count} 条</text>
          {:else if n.kind === 'add' || n.kind === 'agentdefaults' || n.kind === 'botadd'}
            {@const a = n.kind === 'agentdefaults' ? { icon: 'sliders', title: 'Agent 默认值', sub: '所有 agent 没覆盖的项' } : n.data}
            <rect class="card ghost" width={n.w} height={n.h} rx="14" />
            <circle class="disc" cx={n.h / 2} cy={n.h / 2} r={n.h > 50 ? 15 : 12} />
            <g class="ico"><Icon name={a.icon} x={n.h / 2 - 8} y={n.h / 2 - 8} size={16} /></g>
            <text x={n.h + 4} y={a.sub ? n.h / 2 - 3 : n.h / 2 + 4} class="t1 addt">{a.title}</text>
            {#if a.sub}<text x={n.h + 4} y={n.h / 2 + 14} class="t2">{a.sub}</text>{/if}
          {/if}
          {#if issue}
            <g class="issue {issue.level}" transform="translate({n.kind === 'gateway' ? 104 : n.w - 2} {n.kind === 'gateway' ? 14 : 2})">
              <title>{issue.text}</title>
              <circle r="9" />
              <text y="4">{issue.n ?? (issue.level === 'info' ? '↑' : '!')}</text>
            </g>
          {/if}
        </g>
      {/each}
    </g>
  </svg>

  {#if !ov.sessions?.length}
    <div class="hint">
      <Icon name="chat" size={18} />
      <div>
        <strong>还没有会话</strong>
        <p>在命令行、网页或聊天平台发一条消息，会话就会出现在图上，数据流动时连线会亮起来。</p>
        <button class="btn sm primary" onclick={() => (location.hash = '#/sessions/main')}>在网页里聊一句</button>
      </div>
    </div>
  {/if}

  <div class="legend" class:narrow>
    <span style="--c: var(--c-telegram)">入口</span>
    <i>→</i>
    <span style="--c: var(--c-session)">会话</span>
    <i>→</i>
    <span style="--c: var(--c-agent)">Agent</span>
    <span style="--c: var(--accent-2)">群</span>
    <i>→</i>
    <span style="--c: var(--c-gateway)">网关</span>
    <i>→</i>
    <span style="--c: var(--c-model)">模型</span>
    <span style="--c: var(--c-search)">搜索</span>
    <span style="--c: var(--c-memory)">记忆</span>
    <span style="--c: var(--c-skill)">技能</span>
    <span style="--c: var(--c-kernel)">内核</span>
    <i>→</i>
    <span style="--c: var(--c-kernel)">机器</span>
    <i>→</i>
    <span style="--c: var(--c-dir)">工作目录</span>
    <em>点节点：状态、操作、设置都在右边 · 右键节点：常用操作 · ＋ 节点：添加 · Ctrl/⌘ K：搜索设置 · 拖动节点调整位置 · 双击空白处适应窗口</em>
  </div>

  <div class="tools">
    <button class="btn sm" onclick={() => (store.palette = true)} title="搜索设置、节点、会话（Ctrl/⌘ K）"><Icon name="search" size={14} />搜索设置</button>
    {#if Object.keys(offsets).length}
      <button class="btn sm" onclick={resetLayout} title="把拖动过的节点放回原位"><Icon name="undo" size={14} />重置布局</button>
    {/if}
    <button class="btn sm" onclick={fit} title="适应窗口"><Icon name="fit" size={14} /></button>
  </div>

  <div class="stats">
    <div><b>{num(t.prompt_tokens)}</b><span>输入 tokens</span></div>
    <div><b class="acc">{pct(t.cached_tokens, t.prompt_tokens)}</b><span>缓存命中</span></div>
    <div><b>{num(t.completion_tokens)}</b><span>输出 tokens</span></div>
    <div><b>{t.model_calls}</b><span>模型调用</span></div>
  </div>

  {#if menu}
    <ContextMenu x={menu.x} y={menu.y} title={menu.title} items={menu.items} onclose={() => (menu = null)} />
  {/if}
</div>

{#if mountFor}
  {@const cur = store.session(mountFor.sid) ?? mountFor}
  <div use:portal>
    <WorkspacePicker title="给「{cur.title}」挂载工作目录" mounted={cur.workspaces.map((m) => m.id)}
      onchange={(ids) => act(async () => store.upsertSession(await api.patch<SessionInfo>(sidPath(cur.sid), { workspaces: ids })))}
      onclose={() => (mountFor = null)} />
  </div>
{/if}
{#if browseOn}
  <div use:portal>
    <DirPicker host={browseOn.kind === 'local' ? '' : browseOn.name} onpick={addOn} onclose={() => (browseOn = null)} />
  </div>
{/if}

<style>
  .canvas {
    position: relative;
    flex: 1;
    min-width: 0;
    overflow: hidden;
    cursor: grab;
    user-select: none;
    touch-action: none;
  }
  .canvas:active {
    cursor: grabbing;
  }
  .dotgrid {
    fill: color-mix(in srgb, var(--text) 9%, transparent);
  }
  .colhead {
    fill: var(--muted);
    font-size: 12px;
    font-weight: 600;
    letter-spacing: 1px;
    opacity: 0.75;
  }
  .colhead.center {
    text-anchor: middle;
  }

  /* 连线 */
  .edge {
    fill: none;
    stroke: color-mix(in srgb, var(--c) 42%, transparent);
    stroke-width: 1.6;
    transition: stroke-width 0.15s, stroke 0.15s, opacity 0.15s;
  }
  .edge.dashed {
    stroke-dasharray: 4 6;
    opacity: 0.7;
  }
  .edge.flow {
    stroke: color-mix(in srgb, var(--c) 75%, transparent);
    stroke-dasharray: 3 9;
    animation: flow 0.9s linear infinite;
  }
  .edge.hot {
    stroke: var(--c);
    stroke-width: 2.6;
  }
  .edge.dim {
    opacity: 0.35;
  }
  @keyframes flow {
    to {
      stroke-dashoffset: -24;
    }
  }
  .pulse {
    pointer-events: none;
  }

  /* 节点 */
  .node {
    cursor: pointer;
    outline: none;
    transition: opacity 0.2s;
    animation: fadein 0.45s ease;
  }
  .node.dimmed {
    opacity: 0.45;
  }
  @media (prefers-reduced-motion: reduce) {
    .node {
      animation: none;
    }
  }
  .node.dragging {
    cursor: grabbing;
  }
  .node.dragging .card {
    filter: drop-shadow(0 8px 18px rgba(0, 0, 0, 0.35));
  }
  @keyframes fadein {
    from {
      opacity: 0;
    }
  }
  .card {
    fill: var(--panel-solid);
    stroke: color-mix(in srgb, var(--c) 30%, var(--line));
    stroke-width: 1.2;
    transition: stroke 0.15s, filter 0.2s;
  }
  .node:hover .card,
  .node:focus-visible .card {
    stroke: var(--c);
    filter: drop-shadow(0 0 12px color-mix(in srgb, var(--c) 40%, transparent));
  }
  .node.selected .card {
    stroke: var(--c);
    stroke-width: 2.2;
  }
  .node.gateway.selected .core-edge {
    stroke: #fff;
    stroke-width: 2.5;
  }
  .card.off {
    stroke-dasharray: 5 5;
    fill: color-mix(in srgb, var(--panel-solid) 55%, transparent);
  }
  .card.active {
    stroke: var(--c);
    animation: breathe 1.6s ease-in-out infinite;
  }
  .card.bad {
    stroke: var(--err);
    stroke-dasharray: 5 5;
  }
  .card.more {
    fill: transparent;
    stroke-dasharray: 4 5;
  }
  .card.ghost {
    fill: color-mix(in srgb, var(--c) 5%, transparent);
    stroke: color-mix(in srgb, var(--c) 45%, var(--line));
    stroke-dasharray: 5 5;
  }
  .card.agent-card {
    fill: color-mix(in srgb, var(--c) 7%, var(--panel-solid));
    stroke: color-mix(in srgb, var(--c) 55%, var(--line));
    stroke-width: 1.5;
  }
  .addt {
    fill: var(--c);
  }
  @keyframes breathe {
    50% {
      filter: drop-shadow(0 0 16px color-mix(in srgb, var(--c) 60%, transparent));
    }
  }
  .stripe {
    fill: var(--c);
  }
  .disc {
    fill: color-mix(in srgb, var(--c) 16%, transparent);
  }
  .ico {
    color: var(--c);
  }
  .avatar {
    fill: color-mix(in srgb, var(--c) 22%, var(--panel-solid));
    stroke: color-mix(in srgb, var(--c) 50%, transparent);
  }
  .initial {
    fill: var(--c);
    font-size: 13px;
    font-weight: 700;
    text-anchor: middle;
  }
  .t1 {
    fill: var(--text);
    font-size: 13.5px;
    font-weight: 600;
  }
  .t1.mono {
    font-family: var(--mono);
    font-size: 12px;
  }
  .t2 {
    fill: var(--muted);
    font-size: 11.5px;
  }
  .t2.live {
    fill: var(--accent-2);
  }
  .t3 {
    fill: var(--c);
    font-size: 11px;
    letter-spacing: 0.4px;
  }
  .errt {
    fill: var(--err);
  }
  .center {
    text-anchor: middle;
  }
  .sdot {
    fill: var(--muted);
  }
  .sdot.online {
    fill: var(--ok);
    filter: drop-shadow(0 0 4px var(--ok));
  }
  .sdot.starting {
    fill: var(--warn);
    animation: blink 1s infinite;
  }
  .sdot.error {
    fill: var(--err);
    filter: drop-shadow(0 0 4px var(--err));
  }
  .sdot.disabled {
    fill: none;
    stroke: var(--muted);
    stroke-width: 1.5;
  }
  .sdot.idle {
    fill: none;
    stroke: var(--ok);
    stroke-width: 1.5;
  }
  .ring {
    fill: none;
    stroke: var(--accent);
    stroke-width: 2;
    stroke-dasharray: 10 8;
    animation: ringflow 1.2s linear infinite;
  }
  .ring.thinking {
    stroke: var(--c-model);
  }
  .ring.tool {
    stroke: var(--c-kernel);
  }
  .ring.writing {
    stroke: var(--accent-2);
  }
  .ring.pending {
    stroke: var(--warn);
    stroke-dasharray: none;
    animation: blink 1.1s infinite;
  }
  @keyframes ringflow {
    to {
      stroke-dashoffset: -36;
    }
  }
  .badge {
    fill: var(--warn);
  }
  .badge-t {
    fill: #241a00;
    font-size: 12px;
    font-weight: 800;
    text-anchor: middle;
  }
  /* 节点右上角的标记 */
  .issue circle {
    stroke: var(--panel-solid);
    stroke-width: 2;
  }
  .issue.err circle {
    fill: var(--err);
  }
  .issue.warn circle {
    fill: var(--warn);
  }
  .issue.info circle {
    fill: var(--accent);
  }
  .issue text {
    fill: #fff;
    font-size: 11px;
    font-weight: 800;
    text-anchor: middle;
  }
  .issue.warn text {
    fill: #241a00;
  }

  /* 网关 */
  .halo {
    fill: color-mix(in srgb, var(--accent) 18%, transparent);
  }
  .orbit {
    animation: spin 22s linear infinite;
  }
  .orbit-ring {
    fill: none;
    stroke: color-mix(in srgb, var(--accent) 40%, transparent);
    stroke-width: 1.2;
    stroke-dasharray: 2 6;
  }
  .sat.a {
    fill: var(--c-model);
  }
  .sat.b {
    fill: var(--c-kernel);
  }
  .core {
    fill: url(#core);
  }
  .core-edge {
    fill: none;
    stroke: color-mix(in srgb, #fff 30%, transparent);
    stroke-width: 1;
  }
  .node.gateway:hover .core {
    filter: drop-shadow(0 0 18px var(--accent));
  }
  .gw-title {
    fill: #fff;
    font-size: 21px;
    font-weight: 700;
    letter-spacing: 1px;
    text-anchor: middle;
  }
  .gw-sub {
    fill: rgba(255, 255, 255, 0.75);
    font-size: 11.5px;
    text-anchor: middle;
  }
  .gw-stat {
    fill: var(--text-2);
    font-size: 12px;
    text-anchor: middle;
  }

  /* 浮层 */
  .hint {
    position: absolute;
    left: 50%;
    top: 22px;
    transform: translateX(-50%);
    display: flex;
    gap: 12px;
    max-width: 440px;
    padding: 14px 16px;
    border-radius: 14px;
    background: var(--panel);
    border: 1px solid var(--line-2);
    box-shadow: var(--shadow);
    color: var(--accent-2);
    backdrop-filter: blur(10px);
    animation: rise 0.4s ease;
  }
  .hint strong {
    color: var(--text);
  }
  .hint p {
    margin: 4px 0 10px;
    color: var(--text-2);
    font-size: 13px;
  }
  .legend {
    position: absolute;
    left: 18px;
    right: 260px;
    bottom: 14px;
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 7px;
    font-size: 11.5px;
    color: var(--muted);
    pointer-events: none;
  }
  .legend span {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    color: var(--text-2);
  }
  .legend span::before {
    content: '';
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--c);
  }
  .legend i {
    font-style: normal;
    opacity: 0.6;
  }
  .legend em {
    font-style: normal;
    flex-basis: 100%;
    opacity: 0.8;
  }
  .legend.narrow em {
    display: none;
  }
  .legend.narrow {
    right: 200px;
    font-size: 11px;
  }
  .tools {
    position: absolute;
    right: 16px;
    bottom: 14px;
    display: flex;
    gap: 6px;
  }
  .stats {
    position: absolute;
    left: 18px;
    top: 16px;
    display: flex;
    gap: 2px;
    padding: 4px;
    border-radius: 12px;
    background: var(--panel);
    border: 1px solid var(--line);
    backdrop-filter: blur(10px);
  }
  .stats div {
    display: flex;
    flex-direction: column;
    padding: 4px 12px;
  }
  .stats b {
    font-size: 16px;
    font-variant-numeric: tabular-nums;
  }
  .stats b.acc {
    color: var(--accent-2);
  }
  .stats span {
    font-size: 11px;
    color: var(--muted);
  }
</style>
