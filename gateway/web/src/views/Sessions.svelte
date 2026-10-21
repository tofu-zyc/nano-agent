<script lang="ts">
  import { store } from '../lib/state.svelte';
  import { ago, channelMeta, modeMeta } from '../lib/format';
  import Conversation from '../components/Conversation.svelte';
  import Icon from '../components/Icon.svelte';

  let q = $state('');
  let filter = $state('all');

  const sessions = $derived(
    (store.ov?.sessions ?? [])
      .filter((s) => filter === 'all' || s.channel === filter)
      .filter((s) => !q || (s.title + ' ' + s.sid).toLowerCase().includes(q.toLowerCase())),
  );
  const current = $derived(store.route.arg);
  const counts = $derived.by(() => {
    const m: Record<string, number> = {};
    for (const s of store.ov?.sessions ?? []) m[s.channel] = (m[s.channel] ?? 0) + 1;
    return m;
  });

  function newChat() {
    location.hash = '#/sessions/' + encodeURIComponent('web:chat-' + Date.now().toString(36).slice(-4));
  }
</script>

<div class="sessions">
  <aside class="list">
    <div class="top">
      <div class="search">
        <Icon name="search" size={14} />
        <input bind:value={q} placeholder="搜索会话" />
      </div>
      <button class="btn sm primary" onclick={newChat} title="新建网页会话"><Icon name="plus" size={14} /></button>
    </div>
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
        {@const m = channelMeta[s.channel]}
        <a class="item" class:active={current === s.sid} href="#/sessions/{encodeURIComponent(s.sid)}" style="--c: {m?.color ?? 'var(--c-session)'}">
          <div class="av"><Icon name={m?.icon ?? 'chat'} size={15} /></div>
          <div class="txt">
            <div class="t">
              <span class="name">{s.title}</span>
              <span class="when">{s.last_at ? ago(s.last_at * 1000) : ''}</span>
            </div>
            <div class="s">
              {#if store.phase[s.sid]}
                <span class="live">● 处理中</span>
              {:else}
                {modeMeta[s.mode]?.label ?? s.mode} · {s.messages} 条消息
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
      <p class="muted">每个聊天窗口（私聊、群聊、终端、网页）都是一个会话，有自己的上下文、工作目录和权限模式。</p>
      <button class="btn primary" onclick={() => (location.hash = '#/sessions/' + encodeURIComponent('web:main'))}>
        <Icon name="chat" size={14} />打开网页聊天
      </button>
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
    gap: 8px;
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
    max-width: 420px;
    margin: 0 0 8px;
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
