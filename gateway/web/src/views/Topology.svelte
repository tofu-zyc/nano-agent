<script lang="ts">
  import { onMount } from 'svelte';
  import { store } from '../lib/state.svelte';
  import { api } from '../lib/api';
  import { ago, channelMeta, homeShort, modeMeta, num, pct, shortPath, stateLabel, truncate } from '../lib/format';
  import Icon from '../components/Icon.svelte';
  import type { Ev, SessionInfo, Target } from '../lib/types';

  type Kind = 'channel' | 'session' | 'gateway' | 'model' | 'kernel' | 'dir' | 'more';
  type N = { id: string; kind: Kind; x: number; y: number; w: number; h: number; color: string; target?: Target; data?: any };
  type E = { id: string; from: string; to: string; color: string; dashed?: boolean; flow?: boolean };

  const MAX_SESSIONS = 9;
  const ORDER = ['cli', 'web', 'telegram', 'feishu'];
  const X = { chan: 0, sess: 290, gw: 640, right: 850, dir: 1170 };

  const layout = $derived.by(() => {
    const ov = store.ov!;
    const nodes: N[] = [];
    const edges: E[] = [];

    const all = (ov.sessions ?? []).slice();
    // 先挑出最值得看的会话（在跑的、待审批的、最近活跃的），再按渠道排，连线少交叉
    all.sort((a, b) => Number(b.running || b.pending > 0) - Number(a.running || a.pending > 0) || b.last_at - a.last_at);
    const shown = all.slice(0, MAX_SESSIONS);
    shown.sort((a, b) => ORDER.indexOf(a.channel) - ORDER.indexOf(b.channel) || b.last_at - a.last_at);
    const more = all.length - shown.length;

    const chans = ov.channels;
    const nS = shown.length + (more > 0 ? 1 : 0);
    const H = Math.max(600, nS * 64 + 120, chans.length * 96 + 120);
    const cy = H / 2;

    chans.forEach((c, i) => {
      const y = cy + (i - (chans.length - 1) / 2) * 96;
      const meta = channelMeta[c.name] ?? { label: c.name, color: 'var(--c-session)', icon: 'link' };
      nodes.push({ id: 'c:' + c.name, kind: 'channel', x: X.chan, y: y - 32, w: 200, h: 64, color: meta.color,
        target: { type: 'channel', name: c.name }, data: { ...c, meta } });
    });

    shown.forEach((s, j) => {
      const y = cy + (j - (nS - 1) / 2) * 64;
      const color = channelMeta[s.channel]?.color ?? 'var(--c-session)';
      nodes.push({ id: 's:' + s.sid, kind: 'session', x: X.sess, y: y - 24, w: 220, h: 48, color,
        target: { type: 'session', sid: s.sid }, data: s });
      edges.push({ id: `c:${s.channel}>s:${s.sid}`, from: 'c:' + s.channel, to: 's:' + s.sid, color, flow: s.running });
      edges.push({ id: `s:${s.sid}>gw`, from: 's:' + s.sid, to: 'gw', color: 'var(--c-session)', flow: s.running });
    });
    if (more > 0) {
      const y = cy + (nS - 1 - (nS - 1) / 2) * 64;
      nodes.push({ id: 'more', kind: 'more', x: X.sess, y: y - 20, w: 220, h: 40, color: 'var(--muted)', data: more });
    }
    // 没有会话的已启用渠道直接连到网关，表示“接上了”
    for (const c of chans) {
      if (c.status.state !== 'disabled' && !shown.some((s) => s.channel === c.name)) {
        edges.push({ id: `c:${c.name}>gw`, from: 'c:' + c.name, to: 'gw', color: channelMeta[c.name]?.color ?? 'var(--muted)', dashed: true });
      }
    }

    nodes.push({ id: 'gw', kind: 'gateway', x: X.gw - 62, y: cy - 62, w: 124, h: 124, color: 'var(--c-gateway)', target: { type: 'gateway' } });
    nodes.push({ id: 'model', kind: 'model', x: X.right, y: cy - 118, w: 250, h: 80, color: 'var(--c-model)', target: { type: 'model' } });
    nodes.push({ id: 'kernel', kind: 'kernel', x: X.right, y: cy + 38, w: 250, h: 80, color: 'var(--c-kernel)', target: { type: 'kernel' } });
    edges.push({ id: 'gw>model', from: 'gw', to: 'model', color: 'var(--c-model)' });
    edges.push({ id: 'gw>kernel', from: 'gw', to: 'kernel', color: ov.kernel ? 'var(--c-kernel)' : 'var(--err)', dashed: !ov.kernel });

    // 工作目录：显示出来的会话用到的，加上默认工作目录
    const dirs = new Map<string, { sessions: string[]; ro: boolean; def: boolean }>();
    const add = (d: string, sid: string | null, ro: boolean) => {
      const cur = dirs.get(d) ?? { sessions: [], ro: true, def: false };
      if (sid && !cur.sessions.includes(sid)) cur.sessions.push(sid);
      if (!ro) cur.ro = false;
      if (!sid) cur.def = true;
      dirs.set(d, cur);
    };
    for (const d of ov.workspace.dirs ?? []) add(d, null, false);
    for (const s of all) {
      for (const d of s.dirs ?? []) add(d, s.sid, false);
      for (const d of s.ro ?? []) add(d, s.sid, true);
    }
    const list = [...dirs.entries()].slice(0, 7);
    const kcy = cy + 78;
    list.forEach(([path, info], i) => {
      const y = kcy + (i - (list.length - 1) / 2) * 56;
      nodes.push({ id: 'd:' + path, kind: 'dir', x: X.dir, y: y - 22, w: 210, h: 44, color: 'var(--c-dir)',
        target: { type: 'dir', path }, data: { path, ...info } });
      edges.push({ id: `kernel>d:${path}`, from: 'kernel', to: 'd:' + path, color: 'var(--c-dir)', dashed: !ov.kernel });
    });
    if (list.length === 0) {
      nodes.push({ id: 'd:none', kind: 'dir', x: X.dir, y: kcy - 22, w: 210, h: 44, color: 'var(--muted)',
        target: { type: 'workspace' }, data: { path: '', sessions: [], none: true } });
      edges.push({ id: 'kernel>d:none', from: 'kernel', to: 'd:none', color: 'var(--muted)', dashed: true });
    }

    const byId = new Map(nodes.map((n) => [n.id, n]));
    return { nodes, edges, byId, W: X.dir + 210, H, shownSids: new Set(shown.map((s) => s.sid)) };
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
  function onDown(e: PointerEvent) {
    if ((e.target as Element).closest('.node')) return;
    drag = { x: e.clientX, y: e.clientY, vx: view.x, vy: view.y };
    el.setPointerCapture(e.pointerId);
  }
  function onMove(e: PointerEvent) {
    if (!drag) return;
    view = { ...view, x: drag.vx + e.clientX - drag.x, y: drag.vy + e.clientY - drag.y };
    userMoved = true;
  }
  function onUp() {
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

  function onEvent(e: Ev) {
    const sid = e.sid ?? '';
    const s = store.session(sid);
    const ch = s?.channel ?? sid.split(':')[0];
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
        const dir = s?.dirs?.[0];
        chain(dir ? [['gw>kernel', false], [`kernel>d:${dir}`, false]] : [['gw>kernel', false]], 'var(--c-kernel)', 600);
        break;
      }
      case 'tool_result':
        pulse('gw>kernel', e.ok ? 'var(--c-kernel)' : 'var(--err)', { reverse: true, dur: 600 });
        break;
      case 'say':
        if (sessEdge(sid)) chain([[`s:${sid}>gw`, true], [`c:${ch}>s:${sid}`, true]], chColor);
        else pulse(`c:${ch}>gw`, chColor, { reverse: true });
        break;
      case 'run':
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
  const kernelBusy = $derived(phases.includes('tool'));
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

  function sessSub(s: SessionInfo) {
    const ph = store.phase[s.sid];
    if (ph === 'thinking') return '正在思考…';
    if (ph === 'writing') return '正在回复…';
    if (ph === 'tool') return '正在执行工具…';
    if (s.running) return '处理中…';
    return `${modeMeta[s.mode]?.label ?? s.mode} · ${s.last_at ? ago(s.last_at * 1000) : '新会话'}`;
  }

  async function startWebChat() {
    location.hash = '#/sessions/' + encodeURIComponent('web:main');
  }

  function activate(n: N) {
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
          class:dimmed={hover !== null && hover !== n.id && !layout.edges.some((e) => (e.from === hover && e.to === n.id) || (e.to === hover && e.from === n.id))}
          transform="translate({n.x} {n.y})"
          style="--c: {n.color}"
          role="button"
          tabindex="0"
          onclick={() => activate(n)}
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
            <text x="24" y="29" class="initial">{Array.from(s.title.replace(/^.*?·\s*/, ''))[0] ?? '?'}</text>
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
            <rect class="card" class:active={modelBusy} width={n.w} height={n.h} rx="16" />
            <circle class="disc" cx="36" cy="40" r="20" />
            <g class="ico"><Icon name="sparkles" x={24} y={28} size={24} /></g>
            <text x="66" y="28" class="t3">模型 · {ov.model.provider}</text>
            <text x="66" y="48" class="t1">{truncate(ov.model.model || '未配置', 19)}</text>
            <text x="66" y="66" class="t2">{truncate(hostOf(ov.model.base_url) || '—', 16)} · 思考 {ov.model.thinking || '默认'}</text>
          {:else if n.kind === 'kernel'}
            <rect class="card" class:active={kernelBusy} class:bad={!ov.kernel} width={n.w} height={n.h} rx="16" />
            <circle class="disc" cx="36" cy="40" r="20" />
            <g class="ico"><Icon name="shield" x={24} y={28} size={24} /></g>
            <text x="66" y="28" class="t3">内核</text>
            {#if ov.kernel}
              <text x="66" y="48" class="t1">{truncate(ov.kernel.node, 18)}</text>
              <text x="66" y="66" class="t2">{ov.kernel.landlock > 0 ? `Landlock v${ov.kernel.landlock} 沙箱` : '没有沙箱'} · {ov.kernel.tools.length} 个工具</text>
            {:else}
              <text x="66" y="48" class="t1 errt">未连接</text>
              <text x="66" y="66" class="t2">启动 nano-kernel 后自动连上</text>
            {/if}
            <circle cx={n.w - 16} cy="16" r="4" class="sdot {ov.kernel ? 'online' : 'error'}" />
          {:else if n.kind === 'dir'}
            {@const d = n.data}
            <rect class="card" class:off={d.none} width={n.w} height={n.h} rx="12" />
            <g class="ico small"><Icon name="folder" x={13} y={12} size={20} /></g>
            {#if d.none}
              <text x="42" y="20" class="t1">还没有工作目录</text>
              <text x="42" y="35" class="t2">点击设置默认工作目录</text>
            {:else}
              <text x="42" y="20" class="t1 mono">{shortPath(homeShort(d.path), 22)}</text>
              <text x="42" y="35" class="t2">{d.def ? '默认 · ' : ''}{d.sessions.length} 个会话{d.ro ? ' · 只读' : ''}</text>
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
    <span style="--c: var(--c-dir)">工作目录</span>
    <em>点节点查看和修改设置 · 滚轮缩放 · 拖动平移 · 双击空白处复位</em>
  </div>

  <div class="tools">
    <button class="btn sm" onclick={fit} title="适应窗口"><Icon name="fit" size={14} /></button>
  </div>

  <div class="stats">
    <div><b>{num(t.prompt_tokens)}</b><span>输入 tokens</span></div>
    <div><b class="acc">{pct(t.cached_tokens, t.prompt_tokens)}</b><span>缓存命中</span></div>
    <div><b>{num(t.completion_tokens)}</b><span>输出 tokens</span></div>
    <div><b>{t.model_calls}</b><span>模型调用</span></div>
  </div>
</div>

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
