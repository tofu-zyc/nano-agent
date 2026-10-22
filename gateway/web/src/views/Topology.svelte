<script lang="ts">
  import { onMount } from 'svelte';
  import { store } from '../lib/state.svelte';
  import { api, sidPath } from '../lib/api';
  import { ago, channelMeta, homeShort, machineMeta, modeMeta, num, pct, sessionChannels, shortPath, stateLabel, truncate } from '../lib/format';
  import Icon from '../components/Icon.svelte';
  import ContextMenu, { type MenuItem } from '../components/ContextMenu.svelte';
  import WorkspacePicker from '../components/WorkspacePicker.svelte';
  import DirPicker from '../components/DirPicker.svelte';
  import type { Ev, KernelInfo, MachineInfo, MountInfo, SessionInfo, Target, WorkspaceInfo } from '../lib/types';

  type Kind = 'channel' | 'session' | 'gateway' | 'model' | 'kernel' | 'machine' | 'workspace' | 'more';
  type N = { id: string; kind: Kind; x: number; y: number; w: number; h: number; color: string; target?: Target; data?: any };
  type E = { id: string; from: string; to: string; color: string; dashed?: boolean; flow?: boolean };

  const MAX_SESSIONS = 9;
  const ORDER = ['cli', 'web', 'telegram', 'feishu'];
  const X = { chan: 0, sess: 290, gw: 640, right: 850, mach: 1170, ws: 1490 };

  // 拖动过的节点记下偏移量（存在浏览器里，下次打开还是这样摆）
  function loadOffsets(): Record<string, { x: number; y: number }> {
    try {
      return JSON.parse(localStorage.getItem('nano.layout') || '{}');
    } catch {
      return {};
    }
  }
  let offsets = $state<Record<string, { x: number; y: number }>>(loadOffsets());
  function saveOffsets() {
    try {
      localStorage.setItem('nano.layout', JSON.stringify(offsets));
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

  const layout = $derived.by(() => {
    const ov = store.ov!;
    const nodes: N[] = [];
    const edges: E[] = [];

    const all = (ov.sessions ?? []).slice();
    // 先挑出最值得看的会话（在跑的、待审批的、最近活跃的），再按渠道排，连线少交叉
    all.sort((a, b) => Number(b.running || b.pending > 0) - Number(a.running || a.pending > 0) || b.last_at - a.last_at);
    const shown = all.slice(0, MAX_SESSIONS);
    const rank = (s: SessionInfo) => (s.sid === 'main' ? -1 : ORDER.indexOf(sessionChannels(s)[0]));
    shown.sort((a, b) => rank(a) - rank(b) || b.last_at - a.last_at);
    const more = all.length - shown.length;

    const chans = ov.channels;
    const nS = shown.length + (more > 0 ? 1 : 0);
    const H = Math.max(600, nS * 64 + 120, chans.length * 96 + 120, 2 * (70 + Math.max(1, store.machines.length, ov.kernels?.length ?? 0) * 96));
    const cy = H / 2;

    chans.forEach((c, i) => {
      const y = cy + (i - (chans.length - 1) / 2) * 96;
      const meta = channelMeta[c.name] ?? { label: c.name, color: 'var(--c-session)', icon: 'link' };
      nodes.push({ id: 'c:' + c.name, kind: 'channel', x: X.chan, y: y - 32, w: 200, h: 64, color: meta.color,
        target: { type: 'channel', name: c.name }, data: { ...c, meta } });
    });

    // 会话连到它接着的窗口所在的渠道（一个会话可以接好几个渠道的窗口）；没有窗口接着的只能在网页里聊
    shown.forEach((s, j) => {
      const y = cy + (j - (nS - 1) / 2) * 64;
      const color = s.sid === 'main' ? 'var(--accent)' : (channelMeta[s.channel]?.color ?? 'var(--c-session)');
      nodes.push({ id: 's:' + s.sid, kind: 'session', x: X.sess, y: y - 24, w: 220, h: 48, color,
        target: { type: 'session', sid: s.sid }, data: s });
      for (const ch of sessionChannels(s)) {
        const cc = channelMeta[ch]?.color ?? 'var(--c-session)';
        edges.push({ id: `c:${ch}>s:${s.sid}`, from: 'c:' + ch, to: 's:' + s.sid, color: cc, flow: s.running });
      }
      edges.push({ id: `s:${s.sid}>gw`, from: 's:' + s.sid, to: 'gw', color: 'var(--c-session)', flow: s.running });
    });
    if (more > 0) {
      const y = cy + (nS - 1 - (nS - 1) / 2) * 64;
      nodes.push({ id: 'more', kind: 'more', x: X.sess, y: y - 20, w: 220, h: 40, color: 'var(--muted)', data: more });
    }
    // 没有会话的已启用渠道直接连到网关，表示“接上了”
    for (const c of chans) {
      if (c.status.state !== 'disabled' && !shown.some((s) => sessionChannels(s).includes(c.name))) {
        edges.push({ id: `c:${c.name}>gw`, from: 'c:' + c.name, to: 'gw', color: channelMeta[c.name]?.color ?? 'var(--muted)', dashed: true });
      }
    }

    nodes.push({ id: 'gw', kind: 'gateway', x: X.gw - 62, y: cy - 62, w: 124, h: 124, color: 'var(--c-gateway)', target: { type: 'gateway' } });
    nodes.push({ id: 'model', kind: 'model', x: X.right, y: cy - 118, w: 250, h: 80, color: 'var(--c-model)', target: { type: 'model' } });
    edges.push({ id: 'gw>model', from: 'gw', to: 'model', color: 'var(--c-model)' });

    // 内核：连着网关的 agent，各自跑在某台机器上（网关启动的本机内核排在最前面）
    const kernels: KernelInfo[] = (ov.kernels ?? []).slice().sort((a, b) =>
      Number(!!b.managed) - Number(!!a.managed) || Number(b.local) - Number(a.local) || a.node.localeCompare(b.node));
    const top = cy + 38;
    kernels.forEach((k, i) => {
      const id = 'k:' + k.node;
      nodes.push({ id, kind: 'kernel', x: X.right, y: top + i * 86, w: 250, h: 70, color: 'var(--c-kernel)',
        target: { type: 'kernel', node: k.node }, data: k });
      edges.push({ id: `gw>${id}`, from: 'gw', to: id, color: 'var(--c-kernel)', flow: busyKernels.has(k.node) });
    });
    if (!kernels.length) {
      nodes.push({ id: 'k:none', kind: 'kernel', x: X.right, y: top, w: 250, h: 70, color: 'var(--muted)', target: { type: 'kernel' }, data: null });
      edges.push({ id: 'gw>k:none', from: 'gw', to: 'k:none', color: 'var(--err)', dashed: true });
    }

    // 机器：本机（网关所在的）、装了内核的电脑、SSH 服务器。内核连到它所在的机器；SSH 服务器由网关直接连
    // 有内核的机器按内核的顺序排在前面，SSH 服务器和不在线的放后面，连线少交叉
    const kidx = (m: MachineInfo) => {
      const i = kernels.findIndex((k) => (k.local ? '本机' : k.node) === m.name);
      return i < 0 ? kernels.length + (m.kind === 'ssh' ? 0 : 1) : i;
    };
    const machines: MachineInfo[] = store.machines.slice().sort((a, b) => kidx(a) - kidx(b));
    const mtop = top + Math.max(0, (Math.max(1, kernels.length) * 86 - machines.length * 96) / 2);
    machines.forEach((m, i) => {
      const id = 'm:' + m.name;
      nodes.push({ id, kind: 'machine', x: X.mach, y: mtop + i * 96, w: 250, h: 80, color: m.online ? 'var(--c-kernel)' : 'var(--muted)',
        target: { type: 'machine', name: m.name }, data: m });
      const ks = kernels.filter((k) => (k.local ? '本机' : k.node) === m.name);
      for (const k of ks) edges.push({ id: `k:${k.node}>${id}`, from: 'k:' + k.node, to: id, color: 'var(--c-kernel)' });
      if (m.kind === 'ssh' || (m.kind === 'local' && !ks.length)) {
        // 网关自己执行：SSH 服务器，或者本机没有内核时
        edges.push({ id: `gw>${id}`, from: 'gw', to: id, color: m.online ? 'var(--accent-2)' : 'var(--err)', dashed: !m.online });
      }
    });

    const order = (h: string) => machines.findIndex((m) => m.name === h);
    const list = (ov.workspaces ?? []).filter((w) => order(w.host) >= 0).sort((a, b) => order(a.host) - order(b.host)).slice(0, 10);
    const mcy = mtop + (machines.length * 96 - 16) / 2;
    list.forEach((w, i) => {
      const y = mcy + (i - (list.length - 1) / 2) * 56;
      nodes.push({ id: 'w:' + w.id, kind: 'workspace', x: X.ws, y: y - 22, w: 210, h: 44, color: 'var(--c-dir)',
        target: { type: 'workspace', id: w.id }, data: w });
      edges.push({ id: `m:${w.host}>w:${w.id}`, from: 'm:' + w.host, to: 'w:' + w.id, color: 'var(--c-dir)', dashed: !w.online });
    });
    if (list.length === 0 && machines.length) {
      const m = 'm:' + machines[0].name;
      nodes.push({ id: 'w:none', kind: 'workspace', x: X.ws, y: mcy - 22, w: 210, h: 44, color: 'var(--muted)',
        target: { type: 'workspace' }, data: null });
      edges.push({ id: `${m}>w:none`, from: m, to: 'w:none', color: 'var(--muted)', dashed: true });
    }

    for (const n of nodes) {
      const o = offsets[n.id];
      if (o) {
        n.x += o.x;
        n.y += o.y;
      }
    }
    const byId = new Map(nodes.map((n) => [n.id, n]));
    return { nodes, edges, byId, W: X.ws + 210, H, shownSids: new Set(shown.map((s) => s.sid)) };
  });

  function anchor(n: N, side: 'l' | 'r') {
    if (n.kind === 'gateway') return { x: n.x + (side === 'l' ? 0 : n.w), y: n.y + n.h / 2 };
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
  let hover = $state<string | null>(null);

  function fit() {
    if (!el) return;
    const r = el.getBoundingClientRect();
    const pad = 48;
    const k = Math.min((r.width - pad * 2) / layout.W, (r.height - pad * 2) / layout.H, 1.15);
    view = { k, x: (r.width - layout.W * k) / 2, y: (r.height - layout.H * k) / 2 };
    userMoved = false;
  }

  $effect(() => {
    layout.W;
    layout.H;
    if (!userMoved) queueMicrotask(fit);
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
    const g = (e.target as Element).closest('.node') as SVGGElement | null;
    if (g) {
      // 拖节点时不捕获指针：否则松手时的点击事件落到画布上，节点就点不开了
      const id = g.dataset.id!;
      const o = offsets[id] ?? { x: 0, y: 0 };
      nodeDrag = { id, x: e.clientX, y: e.clientY, ox: o.x, oy: o.y, moved: false };
    } else {
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

  function sessEdge(sid: string) {
    return layout.shownSids.has(sid);
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
    switch (e.t) {
      case 'message':
        if (sessEdge(sid)) chain([[`c:${ch}>s:${sid}`, false], [`s:${sid}>gw`, false]], chColor);
        else pulse(`c:${ch}>gw`, chColor);
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
        if (sessEdge(sid)) pulse(`s:${sid}>gw`, color, { reverse: true, delay: 420, dur: 600, r: 3 });
        break;
      }
      case 'tool': {
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
        if (sessEdge(sid)) chain([[`s:${sid}>gw`, true], [`c:${ch}>s:${sid}`, true]], chColor);
        else pulse(`c:${ch}>gw`, chColor, { reverse: true });
        break;
      case 'run':
        if (e.kind === 'start' && e.data?.channel) origin.set(sid, e.data.channel);
        if (e.kind === 'end') {
          clearInterval(busyModel.get(sid));
          busyModel.delete(sid);
          if (sessEdge(sid)) chain([[`s:${sid}>gw`, true], [`c:${ch}>s:${sid}`, true]], 'var(--accent-2)');
        }
        break;
    }
  }

  onMount(() => {
    const off = store.on(onEvent);
    const ro = new ResizeObserver(() => {
      if (!userMoved) fit();
    });
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
  // 正在执行工具的会话挂着的机器、正在跑任务的内核
  const busyHosts = $derived(
    new Set((store.ov?.sessions ?? []).filter((s) => store.phase[s.sid] === 'tool').flatMap((s) => s.workspaces.map((m) => m.host))),
  );
  const t = $derived(store.ov!.totals);
  const ov = $derived(store.ov!);

  function hot(e: E) {
    return hover !== null && (e.from === hover || e.to === hover);
  }

  function hostOf(u: string) {
    try {
      return new URL(u).host;
    } catch {
      return u;
    }
  }

  function machineSub(m: MachineInfo) {
    if (!m.online) return truncate(m.detail || '现在用不了', 18);
    const box = m.landlock > 0 ? `Landlock v${m.landlock} 沙箱` : '没有沙箱';
    if (m.kind === 'ssh') return `网关直连 · ${box}`;
    if (m.kernels.length) return truncate(`内核 ${m.kernels.join('、')} · ${box}`, 22);
    return `网关执行工具 · ${box}`;
  }

  function sessSub(s: SessionInfo) {
    const ph = store.phase[s.sid];
    if (ph === 'thinking') return '正在思考…';
    if (ph === 'writing') return '正在回复…';
    if (ph === 'tool') return '正在执行工具…';
    if (s.running) return '处理中…';
    const mode = s.workspaces.length ? (modeMeta[s.mode]?.label ?? s.mode) + (s.workspaces.length > 1 ? ` · ${s.workspaces.length} 个目录` : '') : '只聊天';
    return `${mode} · ${s.last_at ? ago(s.last_at * 1000) : '新会话'}`;
  }

  async function startWebChat() {
    location.hash = '#/sessions/main';
  }

  // ---- 右键菜单：常用操作 ----

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
    const d = await api.get<{ connections: { id: string; name: string }[] }>('/api/devices');
    const c = d.connections.find((x) => x.name === name);
    if (!c) throw new Error('没有找到这台服务器的连接');
    await api.post(`/api/devices/${c.id}/${op}`);
  }

  function menuFor(n: N): { title: string; items: MenuItem[] } | null {
    const open: MenuItem = { label: '查看和设置', icon: 'sliders', run: () => n.target && store.open(n.target) };
    switch (n.kind) {
      case 'session': {
        const s = n.data as SessionInfo;
        return { title: '会话 · ' + s.title, items: [
          { label: '打开对话', icon: 'chat', run: () => (location.hash = '#/sessions/' + encodeURIComponent(s.sid)) },
          { label: '挂载 / 取下工作目录…', icon: 'folder', run: () => (mountFor = s) },
          open,
          'sep',
          { label: '停止当前任务', icon: 'stop', disabled: !s.running, run: () => act(() => api.post(`${sidPath(s.sid)}/stop`), '已停止') },
          { label: '清空上下文', icon: 'trash', run: () => confirm(`清空「${s.title}」的上下文？`) && act(() => api.post(`${sidPath(s.sid)}/reset`), '上下文已清空') },
          ...(s.sid === 'main' ? [] : [{ label: '删除会话', icon: 'trash', danger: true,
            run: () => confirm(`删除会话「${s.title}」？历史和设置都会删掉。`) && act(() => api.del(sidPath(s.sid)), '会话已删除') } as MenuItem]),
        ] };
      }
      case 'channel': {
        const c = n.data;
        const on = c.status.state !== 'disabled';
        const toggle: MenuItem[] = c.name === 'web' ? [] : [{ label: on ? '停用' : '启用', icon: on ? 'stop' : 'zap',
          run: () => store.setConfig({ [`${c.name}.enabled`]: !on }) }];
        return { title: c.meta.label, items: [open, ...toggle] };
      }
      case 'kernel': {
        const k = n.data as KernelInfo | null;
        if (!k) return { title: '内核', items: [open, { label: '去“设备”页接入电脑', icon: 'laptop', run: () => store.go('#/devices') }] };
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
          { label: '浏览目录，添加工作目录…', icon: 'folder', disabled: !m.online, run: () => (browseOn = m) },
          open,
        ];
        if (m.kind === 'ssh') {
          items.push('sep',
            m.online
              ? { label: '断开 SSH 连接', icon: 'stop', run: () => act(() => sshAction(m.name, 'disconnect'), '正在断开') }
              : { label: '连接', icon: 'link', run: () => act(() => sshAction(m.name, 'connect'), '正在连接') },
            { label: '在“设备”里管理', icon: 'server', run: () => store.go('#/devices') });
        }
        return { title: (m.kind === 'local' ? '本机 · 网关所在的机器' : machineMeta[m.kind].label + ' · ' + m.name), items };
      }
      case 'workspace': {
        const w = n.data as WorkspaceInfo | null;
        if (!w) return { title: '工作目录', items: [open] };
        const main = store.session('main');
        const mounted = main?.workspaces.some((x) => x.id === w.id);
        return { title: `${w.host}:${w.path}`, items: [
          ...(main ? [{ label: mounted ? '从主会话取下' : '挂到主会话', icon: 'hub',
            run: () => act(async () => store.upsertSession(await api.patch<SessionInfo>(sidPath('main'),
              { workspaces: mounted ? main.workspaces.filter((x) => x.id !== w.id).map((x) => x.id) : [...main.workspaces.map((x) => x.id), w.id] })),
              mounted ? '已从主会话取下' : '已挂到主会话') } as MenuItem] : []),
          { label: w.default ? '新会话不再默认挂它' : '新会话默认挂上它', icon: 'check',
            run: () => act(() => api.put('/api/workspaces/default', { id: w.id, default: !w.default })) },
          open,
          'sep',
          { label: '从列表里删掉', icon: 'trash', danger: true,
            run: () => confirm(`从列表里删掉 ${w.host}:${w.path}？机器上的文件不会动。`) && act(() => api.del(`/api/workspaces/${w.id}`), '已删掉') },
        ] };
      }
      case 'model':
        return { title: '模型', items: [open] };
      case 'gateway':
        return { title: '网关', items: [open, { label: '全部设置', icon: 'sliders', run: () => store.go('#/settings') }] };
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
    await act(() => api.post('/api/workspaces', { host: m.name, path }), `已添加 ${m.name}:${path}`);
  }

  function activate(n: N) {
    if (swallowClick) {
      swallowClick = false;
      return;
    }
    if (n.kind === 'more') location.hash = '#/sessions';
    else if (n.target) store.open(n.target);
  }
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
  ondblclick={(e) => !(e.target as Element).closest('.node') && fit()}
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
        <g
          class="node {n.kind}"
          class:dragging={dragged === n.id}
          data-id={n.id}
          class:dimmed={hover !== null && hover !== n.id && !layout.edges.some((e) => (e.from === hover && e.to === n.id) || (e.to === hover && e.from === n.id))}
          transform="translate({n.x} {n.y})"
          style="--c: {n.color}"
          role="button"
          tabindex="0"
          onclick={() => activate(n)}
          oncontextmenu={(ev) => openMenu(ev, n)}
          ondblclick={() => n.kind === 'session' && (location.hash = '#/sessions/' + encodeURIComponent(n.data.sid))}
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
              {off ? '未启用 · 点击配置' : c.status.state === 'error' ? truncate(c.status.error || '出错', 14) : c.status.identity || stateLabel[c.status.state]}
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
            <rect class="card" class:active={modelBusy} width={n.w} height={n.h} rx="16" />
            <circle class="disc" cx="36" cy="40" r="20" />
            <g class="ico"><Icon name="sparkles" x={24} y={28} size={24} /></g>
            <text x="66" y="28" class="t3">模型 · {de?.kind ?? ov.model.provider}{mn > 1 ? ` · ${mn} 个` : ''}</text>
            <text x="66" y="48" class="t1">{truncate(dm?.label || ov.model.model || '未配置', 19)}</text>
            <text x="66" y="66" class="t2">{truncate(hostOf(de?.base_url || ov.model.base_url) || '—', 16)} · 思考 {(dm?.thinking ?? ov.model.thinking) === 'none' ? '关闭' : dm?.thinking || ov.model.thinking || '默认'}</text>
          {:else if n.kind === 'kernel'}
            {@const k = n.data as KernelInfo | null}
            <rect class="card" class:active={!!k && busyKernels.has(k.node)} class:bad={!k} width={n.w} height={n.h} rx="16" />
            <circle class="disc" cx="34" cy="35" r="18" />
            <g class="ico"><Icon name="shield" x={23} y={24} size={22} /></g>
            {#if k}
              <text x="62" y="24" class="t3">{k.managed ? '内核 · 网关启动的' : k.local ? '内核 · 在本机' : '内核 · 在别的电脑上'}</text>
              <text x="62" y="43" class="t1">{truncate(k.node, 18)}</text>
              <text x="62" y="59" class="t2">{k.landlock > 0 ? `Landlock v${k.landlock} 沙箱` : '没有沙箱'}{busyKernels.has(k.node) ? ' · 正在跑' : ''}</text>
              <circle cx={n.w - 16} cy="16" r="4" class="sdot online" />
            {:else}
              <text x="62" y="31" class="t1 errt">没有内核在线</text>
              <text x="62" y="49" class="t2">网关会在本机启动一个</text>
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
            {@const w = n.data as WorkspaceInfo | null}
            <rect class="card" class:off={!w} width={n.w} height={n.h} rx="12" />
            <g class="ico small"><Icon name="folder" x={13} y={12} size={20} /></g>
            {#if !w}
              <text x="42" y="20" class="t1">还没有工作目录</text>
              <text x="42" y="35" class="t2">点击添加一个</text>
            {:else}
              <text x="42" y="20" class="t1 mono">{shortPath(homeShort(w.path), 22)}</text>
              <text x="42" y="35" class="t2">{w.default ? '默认 · ' : ''}{w.sessions} 个会话</text>
            {/if}
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
        <button class="btn sm primary" onclick={startWebChat}>在网页里聊一句</button>
      </div>
    </div>
  {/if}

  <div class="legend">
    <span style="--c: var(--c-telegram)">渠道</span>
    <i>→</i>
    <span style="--c: var(--c-session)">会话</span>
    <i>→</i>
    <span style="--c: var(--c-gateway)">网关</span>
    <i>→</i>
    <span style="--c: var(--c-model)">模型</span>
    <span style="--c: var(--c-kernel)">内核</span>
    <i>→</i>
    <span style="--c: var(--c-kernel)">机器</span>
    <i>→</i>
    <span style="--c: var(--c-dir)">工作目录</span>
    <em>点节点查看和修改设置 · 右键节点有常用操作 · 拖动节点调整位置 · 滚轮缩放 · 拖动空白处平移 · 双击空白处复位</em>
  </div>

  <div class="tools">
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
  <WorkspacePicker title="给「{cur.title}」挂载工作目录" mounted={cur.workspaces.map((m) => m.id)}
    onchange={(ids) => act(async () => store.upsertSession(await api.patch<SessionInfo>(sidPath(cur.sid), { workspaces: ids })))}
    onclose={() => (mountFor = null)} />
{/if}
{#if browseOn}
  <DirPicker host={browseOn.kind === 'local' ? '' : browseOn.name} onpick={addOn} onclose={() => (browseOn = null)} />
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
    margin-left: 10px;
    opacity: 0.8;
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
