<script lang="ts">
  import { store } from '../lib/state.svelte';
  import { ago, channelMeta, endpointLabel, sessionChannels } from '../lib/format';
  import Conversation from '../components/Conversation.svelte';
  import Icon from '../components/Icon.svelte';

  let q = $state('');
  let filter = $state('all');

  // 新建会话时先选 agent：后端一直支持（newSession(title, agent)），只是界面没给入口
  let picker = $state(false);
  function startNewSession() {
    if (store.agents.length > 1) picker = !picker;
    else store.newSession();
  }
  function newSessionFor(agent: string) {
    picker = false;
    store.newSession('', agent);
  }

  const sessions = $derived(
    (store.ov?.sessions ?? [])
      .filter((s) => filter === 'all' || sessionChannels(s).includes(filter))
      .filter((s) => !q || (s.title + ' ' + s.sid).toLowerCase().includes(q.toLowerCase())),
  );
  const current = $derived(store.route.arg);
  const counts = $derived.by(() => {
    const m: Record<string, number> = {};
    for (const s of store.ov?.sessions ?? []) for (const c of sessionChannels(s)) m[c] = (m[c] ?? 0) + 1;
    return m;
  });
</script>

<div class="sessions">
  <aside class="list">
    <div class="top">
      <div class="search">
        <Icon name="search" size={14} />
        <input bind:value={q} placeholder="搜索会话" />
      </div>
      <button class="btn sm primary" onclick={startNewSession} title="新建会话"><Icon name="plus" size={14} /></button>
    </div>
    {#if picker}
      <button type="button" class="scrim" aria-label="关闭" onclick={() => (picker = false)}></button>
      <div class="newmenu">
        <div class="hint">在哪个 agent 下新建会话？</div>
        {#each store.agents as ag (ag.id)}
          <button onclick={() => newSessionFor(ag.id)}>
            <span class="emoji">{ag.emoji || '🙂'}</span>
            <span class="grow">{ag.name}</span>
            {#if ag.default}<span class="muted small">默认</span>{/if}
          </button>
        {/each}
      </div>
    {/if}
    <div class="filters">
      <button class:active={filter === 'all'} onclick={() => (filter = 'all')}>全部</button>
      {#each Object.entries(channelMeta) as [name, m]}
        {#if counts[name]}
          <button class:active={filter === name} onclick={() => (filter = name)} style="--c: {m.color}">
            <i></i>{m.label}<span>{counts[name]}</span>
          </button>
        {/if}
      {/each}
    </div>
    <div class="items">
      {#each sessions as s (s.sid)}
        {@const main = s.sid === 'main'}
        {@const m = channelMeta[s.channel]}
        <a class="item" class:active={current === s.sid} href="#/sessions/{encodeURIComponent(s.sid)}" style="--c: {main ? 'var(--accent)' : (m?.color ?? 'var(--c-session)')}">
          <div class="av"><Icon name={main ? 'hub' : (m?.icon ?? 'chat')} size={15} /></div>
          <div class="txt">
            <div class="t">
              <span class="name">{s.title}</span>
              <span class="when">{s.last_at ? ago(s.last_at * 1000) : ''}</span>
            </div>
            <div class="s">
              {#if store.phase[s.sid]}
                <span class="live">● 处理中</span>
              {:else}
                <span class="eps">{s.channel === 'life' ? '内心活动 · 只能看' : s.endpoints?.length ? s.endpoints.map(endpointLabel).join('、') : '网页'}</span>
                <span class="n">· {s.messages} 条</span>
              {/if}
              {#if s.pending}<span class="pending">待审批 {s.pending}</span>{/if}
            </div>
          </div>
        </a>
      {:else}
        <div class="none muted">{q ? '没有匹配的会话' : '还没有会话'}</div>
      {/each}
    </div>
  </aside>

  {#if current}
    <Conversation sid={current} />
  {:else}
    <div class="welcome">
      <div class="art">
        <svg viewBox="0 0 200 120" width="200" height="120">
          <path d="M20 60 C60 60 60 25 100 25 S140 60 180 60" class="w1" />
          <path d="M20 60 C60 60 60 95 100 95 S140 60 180 60" class="w2" />
          <circle cx="20" cy="60" r="7" class="n1" />
          <circle cx="100" cy="25" r="5" class="n2" />
          <circle cx="100" cy="95" r="5" class="n3" />
          <circle cx="180" cy="60" r="9" class="n4" />
        </svg>
      </div>
      <h3>选一个会话，或者在网页里直接聊</h3>
      <p class="muted">
        会话是一段对话，有自己的上下文、工作目录和权限模式。终端、和机器人的私聊都是接在某个会话上的窗口：
        你的所有窗口默认都接在主会话上，在哪边说话都是同一段上下文；别人私聊机器人，各自一个会话。
      </p>
      <div class="acts">
        <a class="btn primary" href="#/sessions/main"><Icon name="hub" size={14} />打开主会话</a>
        <button class="btn" onclick={startNewSession}><Icon name="plus" size={14} />新建会话</button>
      </div>
    </div>
  {/if}
</div>

<style>
  .sessions {
    flex: 1;
    min-width: 0;
    display: flex;
  }
  .list {
    position: relative;
    width: 300px;
    flex: none;
    display: flex;
    flex-direction: column;
    border-right: 1px solid var(--line);
    background: color-mix(in srgb, var(--panel-solid) 35%, transparent);
  }
  .top {
    display: flex;
    gap: 8px;
    padding: 12px 12px 8px;
  }
  .search {
    flex: 1;
    display: flex;
    align-items: center;
    gap: 7px;
    padding: 0 10px;
    height: 30px;
    border-radius: 9px;
    background: var(--bg-2);
    border: 1px solid var(--line);
    color: var(--muted);
  }
  .search input {
    flex: 1;
    min-width: 0;
    border: none;
    outline: none;
    background: none;
    font-size: 13px;
  }
    .scrim {
    position: fixed;
    inset: 0;
    z-index: 20;
    border: 0;
    background: transparent;
    cursor: default;
  }
  .newmenu {
    position: absolute;
    top: 46px;
    right: 10px;
    z-index: 21;
    min-width: 200px;
    padding: 6px;
    border-radius: 12px;
    background: var(--panel-solid);
    border: 1px solid var(--line-2);
    box-shadow: 0 14px 30px rgba(0, 0, 0, 0.28);
  }
  .newmenu .hint {
    padding: 4px 8px 6px;
    font-size: 11px;
    color: var(--muted);
  }
  .newmenu button {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    padding: 7px 8px;
    border: 0;
    border-radius: 8px;
    background: none;
    color: var(--fg);
    font-size: 13px;
    text-align: left;
    cursor: pointer;
  }
  .newmenu button:hover {
    background: var(--bg-2);
  }
  .newmenu .grow {
    flex: 1;
  }
  .newmenu .small {
    font-size: 11px;
  }

  .filters {
    display: flex;
    flex-wrap: wrap;
    gap: 5px;
    padding: 0 12px 10px;
    border-bottom: 1px solid var(--line);
  }
  .filters button {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    height: 24px;
    padding: 0 9px;
    border-radius: 12px;
    border: 1px solid var(--line);
    background: none;
    font-size: 12px;
    color: var(--text-2);
  }
  .filters button i {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--c);
  }
  .filters button span {
    color: var(--muted);
  }
  .filters button.active {
    color: var(--text);
    border-color: var(--accent);
    background: color-mix(in srgb, var(--accent) 12%, transparent);
  }
  .items {
    flex: 1;
    overflow: auto;
    padding: 6px;
  }
  .item {
    display: flex;
    gap: 10px;
    padding: 9px 10px;
    border-radius: 11px;
    text-decoration: none;
    color: inherit;
    transition: background 0.12s;
  }
  .item:hover {
    background: var(--panel-2);
  }
  .item.active {
    background: color-mix(in srgb, var(--accent) 14%, transparent);
  }
  .av {
    width: 34px;
    height: 34px;
    flex: none;
    border-radius: 10px;
    display: grid;
    place-items: center;
    color: var(--c);
    background: color-mix(in srgb, var(--c) 15%, transparent);
  }
  .txt {
    flex: 1;
    min-width: 0;
  }
  .t {
    display: flex;
    gap: 8px;
    align-items: baseline;
  }
  .name {
    flex: 1;
    min-width: 0;
    font-weight: 600;
    font-size: 13.5px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .when {
    font-size: 11px;
    color: var(--muted);
    flex: none;
  }
  .s {
    font-size: 12px;
    color: var(--muted);
    display: flex;
    gap: 6px;
    min-width: 0;
  }
  .s .eps {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .s .n {
    flex: none;
  }
  .live {
    color: var(--accent-2);
  }
  .pending {
    color: var(--warn);
  }
  .none {
    padding: 30px 10px;
    text-align: center;
  }
  .welcome {
    flex: 1;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: 10px;
    padding: 30px;
    text-align: center;
  }
  .welcome h3 {
    margin: 10px 0 0;
  }
  .welcome p {
    max-width: 460px;
    margin: 0 0 8px;
    line-height: 1.7;
  }
  .acts {
    display: flex;
    gap: 8px;
  }
  .art path {
    fill: none;
    stroke-width: 2;
    stroke-dasharray: 4 6;
    animation: flow 1.4s linear infinite;
  }
  .w1 {
    stroke: color-mix(in srgb, var(--c-telegram) 60%, transparent);
  }
  .w2 {
    stroke: color-mix(in srgb, var(--c-web) 60%, transparent);
  }
  .n1 {
    fill: var(--c-session);
  }
  .n2 {
    fill: var(--c-model);
  }
  .n3 {
    fill: var(--c-kernel);
  }
  .n4 {
    fill: var(--accent);
  }
  @keyframes flow {
    to {
      stroke-dashoffset: -20;
    }
  }
</style>
