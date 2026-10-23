<script lang="ts">
  import { store } from '../lib/state.svelte';
  import Field from './Field.svelte';
  import Icon from './Icon.svelte';

  let { group, title = '设置', focus = null }: { group: string; title?: string; focus?: string | null } = $props();

  const fields = $derived(store.fields.filter((f) => f.group === group));
  const basic = $derived(fields.filter((f) => !f.advanced));
  const advanced = $derived(fields.filter((f) => f.advanced));
  let showAdvanced = $state(false);

  const sections = $derived.by(() => {
    const out: { name: string; items: typeof fields }[] = [];
    for (const f of basic) {
      const name = f.section ?? '';
      let s = out.find((x) => x.name === name);
      if (!s) out.push((s = { name, items: [] }));
      s.items.push(f);
    }
    return out;
  });

  $effect(() => {
    if (focus && advanced.some((f) => f.key === focus)) showAdvanced = true;
    if (focus) {
      queueMicrotask(() => document.getElementById('f-' + focus)?.scrollIntoView({ block: 'center', behavior: 'smooth' }));
    }
  });
</script>

{#if fields.length}
  <div class="group">
    <h4>{title}<span class="auto">改完按回车或点到别处就自动保存</span></h4>
    {#each sections as s}
      {#if s.name && sections.length > 1}<div class="sec">{s.name}</div>{/if}
      <div class="box">
        {#each s.items as f (f.key)}
          <Field {f} highlight={focus === f.key} />
        {/each}
      </div>
    {/each}
    {#if advanced.length}
      <button class="more" onclick={() => (showAdvanced = !showAdvanced)}>
        <Icon name={showAdvanced ? 'down' : 'right'} size={13} />
        更多设置（{advanced.length}）
      </button>
      {#if showAdvanced}
        <div class="box">
          {#each advanced as f (f.key)}
            <Field {f} highlight={focus === f.key} />
          {/each}
        </div>
      {/if}
    {/if}
  </div>
{/if}

<style>
  .group {
    margin-top: 18px;
  }
  h4 .auto {
    margin-left: 8px;
    font-weight: 400;
    letter-spacing: 0;
    opacity: 0.8;
  }
  h4 {
    margin: 0 0 8px 2px;
    font-size: 12px;
    font-weight: 600;
    letter-spacing: 0.5px;
    color: var(--muted);
  }
  .sec {
    margin: 14px 0 6px 4px;
    font-size: 12px;
    color: var(--text-2);
    font-weight: 600;
  }
  .box {
    border-radius: 14px;
    background: var(--panel-2);
    border: 1px solid var(--line);
  }
  .box :global(.field + .field) {
    border-top: 1px solid var(--line);
    border-radius: 0;
  }
  .more {
    display: flex;
    align-items: center;
    gap: 5px;
    margin: 12px 0 8px;
    border: none;
    background: none;
    color: var(--text-2);
    font-size: 12.5px;
    padding: 0 2px;
  }
  .more:hover {
    color: var(--text);
  }
</style>
