<script lang="ts">
  // Agent 默认值：所有 agent 没覆盖的项都在这里（每个配置项只有一个家）。
  import FieldGroup from '../FieldGroup.svelte';
  import { store } from '../../lib/state.svelte';
  import Icon from '../Icon.svelte';

  const agents = $derived(store.agents);
</script>

<div class="pnl">
  <p class="p-note lead">这里是所有 agent 的默认值：每条消息的上限、权限、管理员、显示档位这些。每个 agent 可以在自己的节点里覆盖其中一部分（模型、思考强度、上限、权限模式、上下文整理），没覆盖的就用这里的。</p>

  <h5 class="p-h">有哪些 agent <span class="muted p-small">点进去改它自己的</span></h5>
  <div class="p-card">
    {#each agents as a (a.id)}
      <button class="p-srow" onclick={() => store.open({ type: 'agent', id: a.id })}>
        <span>{a.emoji || '🙂'}</span>
        <span class="p-grow">{a.name}{#if a.default}<span class="tag accent">默认</span>{/if}</span>
        <span class="muted p-small">{a.sessions} 个会话</span>
        <Icon name="right" size={13} />
      </button>
    {/each}
    <button class="btn sm" onclick={() => store.open({ type: 'agents' })}><Icon name="plus" size={13} />再加一个 agent</button>
  </div>

  <FieldGroup group="agent" focus={store.focus} />
</div>
