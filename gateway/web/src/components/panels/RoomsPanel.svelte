<script lang="ts">
  // ＋ 创建群：选几个 agent 进来，它们在这个群里像同事一样说话。
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import Icon from '../Icon.svelte';

  let name = $state('');
  let mode = $state<'mention' | 'free'>('mention');
  let members = $state<string[]>([]);
  let busy = $state(false);

  function toggle(id: string) {
    members = members.includes(id) ? members.filter((x) => x !== id) : [...members, id];
  }
  async function create() {
    if (!name.trim()) return store.toast({ kind: 'err', text: '给群起个名字' }, 2200);
    if (!members.length) return store.toast({ kind: 'err', text: '至少选一个 agent' }, 2200);
    busy = true;
    try {
      const info = await api.post<{ id: string }>('/api/rooms', { name: name.trim(), mode, members });
      name = '';
      members = [];
      await store.refresh();
      store.inspector = { type: 'room', id: info.id };
      store.toast({ kind: 'ok', text: '建好了，点「打开聊天」就能在里面说话' }, 3200);
    } catch (err) {
      store.fail(err);
    } finally {
      busy = false;
    }
  }
</script>

<div class="pnl">
  <p class="p-note lead">群是网关内部的：选几个 agent 进来，它们在群里像同事一样聊天，人在网页的会话里也能发言。两个 agent 私聊就是在「给某个 agent 发消息」时自动建的两人群。</p>

  <h5 class="p-h">写消息里的工具</h5>
  <div class="p-card">
    <p class="p-note p-small">agent 在干活或者待在群里时，可以用 <code>Message</code> 工具：<code>to: "room:群名"</code> 发到群里，<code>to: "agent:名字"</code> 私聊，<code>wait: true</code> 等对方回话（最多 5 分钟）。</p>
  </div>

  <h5 class="p-h">新建一个群</h5>
  <div class="p-card">
    <label class="p-label">名字
      <input class="p-input" bind:value={name} placeholder="比如 产品组" />
    </label>
    <label class="p-label">怎么说话
      <select class="p-select" bind:value={mode}>
        <option value="mention">点名：被 @ 或者叫到名字才说</option>
        <option value="free">自由：安静下来可以自己插话</option>
      </select>
    </label>
    <div class="members">
      {#each store.agents as a (a.id)}
        <button class="chip" class:on={members.includes(a.id)} onclick={() => toggle(a.id)}>{a.emoji} {a.name}</button>
      {/each}
    </div>
    <div class="p-actions tight">
      <button class="btn sm primary" onclick={create} disabled={busy}><Icon name="plus" size={13} />创建</button>
    </div>
  </div>

  <h5 class="p-h">现在的群</h5>
  <div class="p-card">
    {#each store.rooms as r (r.id)}
      <button class="p-srow" onclick={() => store.open({ type: 'room', id: r.id })}>
        <Icon name="chat" size={14} />
        <span class="p-grow">{r.name}{#if r.paused}<span class="tag">暂停</span>{/if}</span>
        <span class="muted p-small">{r.members.length} 人 · {r.count} 条</span>
        <Icon name="right" size={13} />
      </button>
    {:else}
      <p class="p-note">还没有群。</p>
    {/each}
  </div>
</div>

<style>
  .members {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .chip {
    padding: 4px 10px;
    border-radius: 9px;
    border: 1px solid var(--line);
    background: var(--bg-2);
    color: var(--text-2);
    font: inherit;
    font-size: 12.5px;
    cursor: pointer;
  }
  .chip.on {
    border-color: color-mix(in srgb, var(--accent) 55%, var(--line));
    background: color-mix(in srgb, var(--accent) 12%, transparent);
    color: var(--text);
  }
</style>
