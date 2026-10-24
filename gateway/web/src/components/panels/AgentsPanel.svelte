<script lang="ts">
  // ＋ 添加 agent：新建一个一直存在的 agent，它可以有自己的名字、头像、人格，绑自己的机器人。
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import Icon from '../Icon.svelte';

  let name = $state('');
  let emoji = $state('');
  let busy = $state(false);

  async function create() {
    if (!name.trim()) {
      store.toast({ kind: 'err', text: '先给它起个名字' }, 2400);
      return;
    }
    busy = true;
    try {
      const info = await api.post<{ id: string }>('/api/agents', { name: name.trim(), emoji: emoji.trim() });
      name = '';
      emoji = '';
      await store.refresh();
      store.inspector = { type: 'agent', id: info.id };
      store.toast({ kind: 'ok', text: '建好了：先去写人格，再给它绑个机器人' }, 3200);
    } catch (err) {
      store.fail(err);
    } finally {
      busy = false;
    }
  }
</script>

<div class="pnl">
  <p class="p-note lead">agent 是一直在的个体，不是用完就扔的工具：它有自己的名字、人格、模型和记忆（会话），可以绑自己的机器人。会话 ID 会加上它的前缀，比如 @bob/main。</p>

  <h5 class="p-h">新建一个 agent</h5>
  <div class="p-card">
    <label class="p-label">名字
      <input class="p-input" bind:value={name} placeholder="比如 小鲍" />
    </label>
    <label class="p-label">头像（一个 emoji，可以不填）
      <input class="p-input" bind:value={emoji} placeholder="🦦" />
    </label>
    <div class="p-actions tight">
      <button class="btn sm primary" onclick={create} disabled={busy}><Icon name="plus" size={13} />创建</button>
    </div>
  </div>

  <h5 class="p-h">现在的 agent</h5>
  <div class="p-card">
    {#each store.agents as a (a.id)}
      <button class="p-srow" onclick={() => store.open({ type: 'agent', id: a.id })}>
        <span>{a.emoji || '🙂'}</span>
        <span class="p-grow">{a.name}{#if a.default}<span class="tag accent">默认</span>{/if}</span>
        <span class="muted p-small">{a.id}</span>
        <Icon name="right" size={13} />
      </button>
    {/each}
  </div>
</div>
