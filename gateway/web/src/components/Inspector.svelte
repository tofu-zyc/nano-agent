<script lang="ts">
  // 右侧的节点面板：点了哪个节点就显示哪个节点的全部状态、操作和设置。
  // 面板从上到下是：状态 → 操作 → 设置（按小节，高级的折叠） → 危险操作。左边缘可以拖动调宽度。
  import { store } from '../lib/state.svelte';
  import { channelMeta, homeShort, machineMeta, since, stateLabel } from '../lib/format';
  import FieldGroup from './FieldGroup.svelte';
  import ModelPanel from './ModelPanel.svelte';
  import GatewayPanel from './panels/GatewayPanel.svelte';
  import AgentPanel from './panels/AgentPanel.svelte';
  import AgentDefaultsPanel from './panels/AgentDefaultsPanel.svelte';
  import AgentsPanel from './panels/AgentsPanel.svelte';
  import BotPanel from './panels/BotPanel.svelte';
  import BotsPanel from './panels/BotsPanel.svelte';
  import ChannelPanel from './panels/ChannelPanel.svelte';
  import SessionPanel from './panels/SessionPanel.svelte';
  import SearchPanel from './panels/SearchPanel.svelte';
  import MemoryPanel from './panels/MemoryPanel.svelte';
  import SkillsPanel from './panels/SkillsPanel.svelte';
  import SchedulesPanel from './panels/SchedulesPanel.svelte';
  import KernelPanel from './panels/KernelPanel.svelte';
  import MachinePanel from './panels/MachinePanel.svelte';
  import MachinesPanel from './panels/MachinesPanel.svelte';
  import WorkspacePanel from './panels/WorkspacePanel.svelte';
  import Icon from './Icon.svelte';

  const target = $derived(store.inspector!);
  const ov = $derived(store.ov!);
  const key = $derived(JSON.stringify(target));
  const wide = $derived(target.type === 'model');
  const width = $derived(wide ? Math.max(780, store.panelWidth) : store.panelWidth);
  let bodyEl = $state<HTMLDivElement>();

  // 换了对象：面板从顶部开始
  $effect(() => {
    key;
    bodyEl?.scrollTo({ top: 0 });
  });

  const head = $derived.by((): { icon: string; color: string; title: string; sub: string } => {
    switch (target.type) {
      case 'gateway': {
        const up = ov.runtime?.update;
        return { icon: 'hub', color: 'var(--c-gateway)', title: '网关', sub: `${ov.runtime ? 'v' + ov.runtime.version.replace(/^v/, '') + ' · ' : ''}已运行 ${since(ov.gateway.started)}${up?.available ? ' · 有新版本 ' + up.latest : ''}` };
      }
      case 'agent': {
        const a = store.agent(target.id);
        return { icon: 'persona', color: 'var(--c-agent)', title: a ? (a.emoji ? a.emoji + ' ' + a.name : a.name) : target.id,
          sub: a ? (a.default ? '默认 agent · ' : '') + a.sessions + ' 个会话 · ' + a.bots.length + ' 个机器人' : '这个 agent 不在了' };
      }
      case 'agentDefaults':
        return { icon: 'persona', color: 'var(--c-agent)', title: 'Agent 默认值', sub: '所有 agent 没覆盖的项：上限、权限、管理员、显示档位' };
      case 'agents':
        return { icon: 'plus', color: 'var(--c-agent)', title: '添加 agent', sub: '每个 agent 是图上的一个节点，可以绑自己的机器人' };
      case 'bot': {
        const b = store.bots.find((x) => x.id === target.id);
        return { icon: b?.kind === 'feishu' ? 'feishu' : 'send', color: b?.kind === 'feishu' ? 'var(--c-feishu)' : 'var(--c-telegram)',
          title: b?.name ?? target.id, sub: b ? b.channel + ' · 绑给 ' + (store.agent(b.agent)?.name ?? b.agent) : '这个机器人不在了' };
      }
      case 'bots':
        return { icon: 'plus', color: 'var(--c-telegram)', title: '添加机器人', sub: '一个机器人只绑一个 agent；内置的 Telegram、飞书也能换绑' };
      case 'model': {
        const dm = ov.models?.models?.find((m) => m.default);
        const n = ov.models?.models?.length ?? 0;
        const en = ov.models?.endpoints?.length ?? 0;
        return { icon: 'sparkles', color: 'var(--c-model)', title: '模型', sub: n ? `${n} 个模型 · ${en} 个接口${dm ? ' · 默认 ' + dm.label : ''}` : '还没有配置模型' };
      }
      case 'kernel': {
        if (!target.node) return { icon: 'shield', color: 'var(--err)', title: '内核', sub: '没有内核在线' };
        const k = ov.kernels.find((x) => x.node === target.node);
        return { icon: 'shield', color: k ? 'var(--c-kernel)' : 'var(--muted)', title: target.node, sub: k ? `内核 · ${k.managed ? '网关启动的本机内核' : k.local ? '在本机' : '在别的电脑上'}` : '内核 · 不在线' };
      }
      case 'machine': {
        const m = store.machines.find((x) => x.name === target.name);
        const kind = machineMeta[m?.kind ?? 'kernel'];
        return { icon: kind.icon, color: m?.online ? 'var(--c-kernel)' : 'var(--muted)', title: target.name,
          sub: (m?.kind === 'local' ? '网关所在的机器' : kind.label) + (m?.online ? ' · 在线' : ' · 现在用不了') };
      }
      case 'machines': {
        const n = store.devices?.pairing?.length ?? 0;
        return { icon: 'plus', color: 'var(--c-kernel)', title: '接入机器', sub: n ? `${n} 台电脑在等你确认` : '别的电脑装内核连进来，或者网关去连 SSH 服务器' };
      }
      case 'workspace': {
        if (!target.id) return { icon: 'folder', color: 'var(--c-dir)', title: '工作目录', sub: '机器 + 目录，会话在这里干活' };
        const w = (ov.workspaces ?? []).find((x) => x.id === target.id);
        return { icon: 'folder', color: 'var(--c-dir)', title: w ? homeShort(w.path) : '工作目录', sub: w ? `${w.host} 上的工作目录` : '已经删掉了' };
      }
      case 'channel': {
        const m = channelMeta[target.name];
        const c = ov.channels.find((x) => x.name === target.name);
        return { icon: m?.icon ?? 'link', color: m?.color ?? 'var(--muted)', title: m?.label ?? target.name,
          sub: c ? (c.status.identity ? `${stateLabel[c.status.state]} · ${c.status.identity}` : stateLabel[c.status.state]) : '' };
      }
      case 'search': {
        const si = ov.search;
        const ready = !!si?.enabled && !!si?.key;
        return { icon: 'search', color: ready ? 'var(--c-search)' : 'var(--muted)', title: '网络搜索',
          sub: !si?.enabled ? '已关闭' : ready ? `DeepSeek · 今天搜索 ${si.today.search} 次` : '没有可用的 key' };
      }
      case 'memory':
        return { icon: 'brain', color: 'var(--c-memory)', title: '上下文与记忆', sub: '什么时候整理上下文、整理到多少、归档留多久' };
      case 'skills':
        return { icon: 'book', color: 'var(--c-skill)', title: '技能', sub: `${ov.skills?.list?.length ?? 0} 个技能，所有内核共用` };
      case 'schedules': {
        const sc = ov.schedules;
        return { icon: 'clock', color: 'var(--c-cron)', title: '定时任务', sub: sc?.total ? `${sc.total} 个任务 · ${sc.enabled} 个开着` : '还没有任务' };
      }
      case 'session': {
        const s = store.session(target.sid);
        if (target.sid === 'main') return { icon: 'hub', color: 'var(--accent)', title: s?.title ?? '主会话', sub: '你的所有窗口默认都接在这里' };
        return { icon: 'chat', color: channelMeta[s?.channel ?? '']?.color ?? 'var(--c-session)', title: s?.title ?? target.sid, sub: target.sid };
      }
    }
  });

  // 拖面板左边缘调宽度
  let aside = $state<HTMLElement>();
  function grip(e: PointerEvent) {
    e.preventDefault();
    const right = aside!.getBoundingClientRect().right;
    const move = (ev: PointerEvent) => store.setPanelWidth(right - ev.clientX);
    const up = () => {
      window.removeEventListener('pointermove', move);
      window.removeEventListener('pointerup', up);
    };
    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', up);
  }
</script>

<aside class="insp" bind:this={aside} style="--c: {head.color}; width: {width}px">
  <div class="grip" onpointerdown={grip} ondblclick={() => store.setPanelWidth(440)} title="拖动调宽度，双击恢复" role="separator" aria-orientation="vertical"></div>
  <header>
    <div class="disc"><Icon name={head.icon} size={20} /></div>
    <div class="ht">
      <h3>{head.title}</h3>
      <div class="sub">{head.sub}</div>
    </div>
    <button class="btn ghost sm" onclick={() => (store.inspector = null)} aria-label="关闭"><Icon name="x" size={15} /></button>
  </header>

  <div class="body" bind:this={bodyEl}>
    {#key key}
      {#if target.type === 'gateway'}
        <GatewayPanel />
      {:else if target.type === 'agent'}
        <AgentPanel id={target.id} />
      {:else if target.type === 'agentDefaults'}
        <AgentDefaultsPanel />
      {:else if target.type === 'agents'}
        <AgentsPanel />
      {:else if target.type === 'bot'}
        <BotPanel id={target.id} />
      {:else if target.type === 'bots'}
        <BotsPanel />
      {:else if target.type === 'model'}
        <ModelPanel />
        <FieldGroup group="model" title="所有模型共用的请求参数" focus={store.focus} />
      {:else if target.type === 'search'}
        <SearchPanel />
      {:else if target.type === 'memory'}
        <MemoryPanel />
      {:else if target.type === 'skills'}
        <SkillsPanel />
      {:else if target.type === 'schedules'}
        <SchedulesPanel />
      {:else if target.type === 'kernel'}
        <KernelPanel node={target.node} />
      {:else if target.type === 'machine'}
        <MachinePanel name={target.name} />
      {:else if target.type === 'machines'}
        <MachinesPanel />
      {:else if target.type === 'workspace'}
        <WorkspacePanel id={target.id} />
      {:else if target.type === 'channel'}
        <ChannelPanel name={target.name} />
      {:else if target.type === 'session'}
        <SessionPanel sid={target.sid} />
      {/if}
    {/key}
  </div>
</aside>

<style>
  .insp {
    position: relative;
    max-width: calc(100% - 40px);
    flex: none;
    display: flex;
    flex-direction: column;
    background: var(--panel-solid);
    border-left: 1px solid var(--line-2);
    box-shadow: -10px 0 30px rgba(0, 0, 0, 0.12);
    z-index: 10;
    animation: slide 0.22s ease;
  }
  /* 窄屏：盖在内容上面 */
  @media (max-width: 1000px) {
    .insp {
      position: absolute;
      top: 0;
      right: 0;
      bottom: 0;
      max-width: 100%;
      box-shadow: -18px 0 50px rgba(0, 0, 0, 0.3);
    }
  }
  @keyframes slide {
    from {
      transform: translateX(24px);
      opacity: 0;
    }
  }
  .grip {
    position: absolute;
    left: -4px;
    top: 0;
    bottom: 0;
    width: 8px;
    cursor: col-resize;
    z-index: 2;
  }
  .grip:hover {
    background: linear-gradient(90deg, transparent 3px, color-mix(in srgb, var(--accent) 50%, transparent) 3px 5px, transparent 5px);
  }
  header {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 16px 16px 14px 18px;
    border-bottom: 1px solid var(--line);
    background: linear-gradient(180deg, color-mix(in srgb, var(--c) 10%, transparent), transparent);
  }
  .disc {
    width: 40px;
    height: 40px;
    flex: none;
    border-radius: 12px;
    display: grid;
    place-items: center;
    color: var(--c);
    background: color-mix(in srgb, var(--c) 16%, transparent);
    border: 1px solid color-mix(in srgb, var(--c) 35%, transparent);
  }
  .ht {
    flex: 1;
    min-width: 0;
  }
  h3 {
    margin: 0;
    font-size: 15.5px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .sub {
    font-size: 12px;
    color: var(--muted);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .body {
    flex: 1;
    overflow: auto;
    padding: 16px 16px 40px;
  }
  :global(.spin) {
    animation: spin 0.9s linear infinite;
  }
</style>
