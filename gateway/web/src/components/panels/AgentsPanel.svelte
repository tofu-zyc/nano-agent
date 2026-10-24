<script lang="ts">
  // ＋ 添加 agent：新建一个一直存在的 agent，它可以有自己的名字、头像、人格，绑自己的机器人。
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import type { PersonaPreset } from '../../lib/types';
  import Icon from '../Icon.svelte';

  let name = $state('');
  let emoji = $state('');
  let presetId = $state('');
  let style = $state<'tool' | 'life'>('tool');
  let presets = $state<PersonaPreset[]>([]);
  let busy = $state(false);

  // 建的时候可以直接挑一个自带人格（小纳、大肥鱼），也可以建好再自己写
  api.get<{ items: PersonaPreset[] }>('/api/persona-presets').then((r) => (presets = r.items)).catch(() => (presets = []));

  async function create() {
    if (!name.trim()) {
      store.toast({ kind: 'err', text: '先给它起个名字' }, 2400);
      return;
    }
    busy = true;
    try {
      const info = await api.post<{ id: string }>('/api/agents', { name: name.trim(), emoji: emoji.trim(), style });
      const p = presets.find((x) => x.id === presetId);
      if (p) await api.put('/api/agents/' + encodeURIComponent(info.id) + '/persona', { text: p.text });
      const life = style === 'life';
      name = '';
      emoji = '';
      presetId = '';
      style = 'tool';
      await store.refresh();
      store.inspector = { type: 'agent', id: info.id };
      const L = store.agent(info.id)?.life;
      const tail = !life ? '' : L?.state === 'sleeping' ? '；它是拟人型，现在是它的睡觉时间，醒来以后会自己活动' : '；它是拟人型，过几分钟会第一次自己醒来';
      store.toast({ kind: 'ok', text: (p ? '建好了，人格已经是「' + p.name + '」，随时可以改' : '建好了：先去写人格，再给它绑个机器人') + tail }, 3600);
    } catch (err) {
      store.fail(err);
    } finally {
      busy = false;
    }
  }
</script>

<div class="pnl">
  <p class="p-note lead">agent 是一直在的个体，不是用完就扔的工具：它有自己的名字、人格、模型、会话和长期记忆，可以绑自己的机器人。会话 ID 会加上它的前缀，比如 @bob/main。</p>

  <h5 class="p-h">新建一个 agent</h5>
  <div class="p-card">
    <label class="p-label">名字
      <input class="p-input" bind:value={name} placeholder="比如 小鲍" />
    </label>
    <label class="p-label">头像（一个 emoji，可以不填）
      <input class="p-input" bind:value={emoji} placeholder="🦦" />
    </label>
    <div class="p-label">类型（建好以后也能在它的节点里改）</div>
    <div class="styles" role="radiogroup" aria-label="类型">
      <button class="style" class:on={style === 'tool'} role="radio" aria-checked={style === 'tool'} onclick={() => (style = 'tool')}>
        <span class="si">🛠</span><b>工具型</b><span class="muted p-small">有人找它才干活；群里只回被叫到的</span>
      </button>
      <button class="style" class:on={style === 'life'} role="radio" aria-checked={style === 'life'} onclick={() => (style = 'life')}>
        <span class="si">🌱</span><b>拟人型</b><span class="muted p-small">像人一样：有作息，没事时自己上网、想事情、找人聊天</span>
      </button>
    </div>
    <label class="p-label">人格
      <select class="p-select" bind:value={presetId}>
        <option value="">自己写（建好再去写）</option>
        {#each presets as p (p.id)}
          <option value={p.id}>{p.name} —— {p.desc}</option>
        {/each}
      </select>
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

<style>
  .styles {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 8px;
  }
  .style {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 3px;
    padding: 10px 12px;
    border-radius: 12px;
    border: 1px solid var(--line);
    background: var(--bg-2);
    color: var(--text);
    font: inherit;
    text-align: left;
    cursor: pointer;
  }
  .style .si {
    font-size: 18px;
  }
  .style.on {
    border-color: color-mix(in srgb, var(--accent) 60%, var(--line));
    background: color-mix(in srgb, var(--accent) 10%, var(--bg-2));
  }
  @media (max-width: 420px) {
    .styles {
      grid-template-columns: 1fr;
    }
  }
</style>
