<script lang="ts">
  // 一个房间（网关内部的群聊）：成员、说话方式、暂停、禁言，还能直接打开聊天记录。
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import Icon from '../Icon.svelte';

  let { id }: { id: string } = $props();
  const r = $derived(store.room(id));

  async function patch(body: any) {
    try {
      await api.patch('/api/rooms/' + encodeURIComponent(id), body);
      await store.refresh();
    } catch (err) {
      store.fail(err);
    }
  }
  function toggleMember(agentID: string) {
    if (!r) return;
    const now = r.members.includes(agentID) ? r.members.filter((m) => m !== agentID) : [...r.members, agentID];
    if (!now.length) return store.toast({ kind: 'err', text: '群里至少留一个人' }, 2200);
    patch({ members: now });
  }
  function toggleMuted(agentID: string) {
    if (!r) return;
    const now = r.muted.includes(agentID) ? r.muted.filter((m) => m !== agentID) : [...r.muted, agentID];
    patch({ muted: now });
  }
  async function remove() {
    if (!r || !confirm('删掉群「' + r.name + '」？聊天记录还在会话里。')) return;
    try {
      await api.del('/api/rooms/' + encodeURIComponent(id));
      store.inspector = null;
      await store.refresh();
    } catch (err) {
      store.fail(err);
    }
  }
</script>

{#if r}
  <div class="pnl">
    <div class="p-grid">
      <div class="p-stat"><b>{r.members.length}</b><span>成员</span></div>
      <div class="p-stat"><b>{r.count}</b><span>条消息</span></div>
      <div class="p-stat"><b class:warn={r.paused}>{r.paused ? '暂停' : '在聊'}</b><span>现在</span></div>
    </div>

    <div class="p-card">
      <div class="p-row"><span class="p-k">名字</span><span class="p-grow">{r.name}</span></div>
      <div class="p-row"><span class="p-k">聊天记录</span><span class="p-grow">就是会话「{r.name}」{#if r.dm}<span class="muted p-small">（两个人的私聊）</span>{/if}</span></div>
      <div class="p-actions tight">
        <button class="btn sm primary" onclick={() => { location.hash = '#/sessions/' + encodeURIComponent(r!.sid); store.inspector = null; }}><Icon name="chat" size={13} />打开聊天</button>
        <button class="btn sm" onclick={() => patch({ paused: !r!.paused })}>{r.paused ? '恢复' : '暂停'}</button>
      </div>
      <p class="p-note p-small">在聊天里说话就是在这个群里说话：成员 agent 都看得到，被叫到的会回。连续 8 条 agent 发言或者两个 agent 来回 6 轮，会自动暂停，等你说话。</p>
    </div>

    <h5 class="p-h">成员 <span class="muted p-small">点一下加减；被禁言的只看不说</span></h5>
    <div class="p-card">
      {#each store.agents as a (a.id)}
        <div class="p-row">
          <button class="p-srow p-grow" class:on={r!.members.includes(a.id)} onclick={() => toggleMember(a.id)}>
            <span>{a.emoji || '🙂'}</span>
            <span class="p-grow">{a.name}</span>
            {#if r.members.includes(a.id)}<Icon name="check" size={13} />{/if}
          </button>
          {#if r.members.includes(a.id)}
            <button class="p-lnk p-small" onclick={() => toggleMuted(a.id)}>{r.muted.includes(a.id) ? '解除禁言' : '禁言'}</button>
          {/if}
        </div>
      {/each}
    </div>

    <h5 class="p-h">怎么说话</h5>
    <div class="p-card">
      <select class="p-select" value={r.mode} onchange={(e) => patch({ mode: (e.target as HTMLSelectElement).value })}>
        <option value="mention">点名：只有被 @ 或者叫到名字才说</option>
        <option value="free">自由：安静下来按积极度自己插话</option>
      </select>
      <label class="p-label">名字
        <input class="p-input" value={r.name} onchange={(e) => patch({ name: (e.target as HTMLInputElement).value })} />
      </label>
      <label class="p-label">每天最多用多少 token（AI 的用量单位；0 表示不限，超了自动暂停）
        <input class="p-input" value={r.budget ?? 0} inputmode="numeric"
          onchange={(e) => patch({ budget: Number((e.target as HTMLInputElement).value) || 0 })} />
      </label>
    </div>

    <div class="p-danger-zone">
      <button class="btn sm danger" onclick={remove}><Icon name="trash" size={13} />删掉这个群</button>
      <p class="p-note p-small">聊天记录会留在会话里，随时能看。</p>
    </div>
  </div>
{:else}
  <p class="p-note">没有这个群。</p>
{/if}

<style>
  .p-srow.on {
    border-color: color-mix(in srgb, var(--accent) 55%, var(--line));
    background: color-mix(in srgb, var(--accent) 10%, transparent);
  }
</style>
