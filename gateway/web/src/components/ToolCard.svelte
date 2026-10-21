<script lang="ts">
  import { toolIcon } from '../lib/format';
  import { prettyArgs } from '../lib/conv';
  import Icon from './Icon.svelte';

  let { name, detail, args = '', ok, result, running = false }: {
    name: string; detail: string; args?: string; ok?: boolean; result?: string; running?: boolean;
  } = $props();
  let open = $state(false);
  const lines = $derived(result ? result.replace(/\n$/, '').split('\n').length : 0);
</script>

<div class="tool" class:bad={ok === false} class:open>
  <button class="head" onclick={() => (open = !open)}>
    <span class="ic"><Icon name={toolIcon[name] ?? 'zap'} size={14} /></span>
    <span class="name">{name}</span>
    <span class="detail">{detail}</span>
    {#if running && ok === undefined}
      <span class="st run"><Icon name="refresh" size={13} /></span>
    {:else if ok === false}
      <span class="st err"><Icon name="x" size={13} /></span>
    {:else if ok}
      <span class="st ok"><Icon name="check" size={13} /></span>
    {/if}
    {#if lines > 0}<span class="lines">{lines} 行</span>{/if}
    <Icon name={open ? 'down' : 'right'} size={13} />
  </button>
  {#if open}
    <div class="more">
      {#if args}
        <div class="lbl">参数</div>
        <pre>{prettyArgs(args)}</pre>
      {/if}
      {#if result !== undefined}
        <div class="lbl">结果</div>
        <pre class="res">{result || '（空）'}</pre>
      {/if}
    </div>
  {/if}
</div>

<style>
  .tool {
    border-radius: 11px;
    border: 1px solid var(--line);
    background: color-mix(in srgb, var(--c-kernel) 4%, var(--panel-2));
    overflow: hidden;
    font-size: 12.5px;
    max-width: 100%;
  }
  .tool.bad {
    border-color: color-mix(in srgb, var(--err) 35%, var(--line));
  }
  .head {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 7px 10px;
    border: none;
    background: none;
    text-align: left;
    color: var(--text-2);
  }
  .ic {
    color: var(--c-kernel);
    display: grid;
  }
  .name {
    font-weight: 600;
    color: var(--text);
    font-family: var(--mono);
    font-size: 12px;
  }
  .detail {
    flex: 1;
    min-width: 0;
    font-family: var(--mono);
    font-size: 12px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .st {
    display: grid;
    place-items: center;
    width: 18px;
    height: 18px;
    border-radius: 50%;
  }
  .st.ok {
    color: var(--ok);
    background: color-mix(in srgb, var(--ok) 14%, transparent);
  }
  .st.err {
    color: var(--err);
    background: color-mix(in srgb, var(--err) 14%, transparent);
  }
  .st.run {
    color: var(--c-kernel);
    animation: spin 1s linear infinite;
  }
  .lines {
    font-size: 11px;
    color: var(--muted);
  }
  .more {
    padding: 0 10px 10px;
  }
  .lbl {
    font-size: 11px;
    color: var(--muted);
    margin: 4px 0;
  }
  pre {
    margin: 0;
    padding: 8px 10px;
    border-radius: 8px;
    background: var(--bg-2);
    font-size: 12px;
    line-height: 1.5;
    white-space: pre-wrap;
    word-break: break-all;
    max-height: 320px;
    overflow: auto;
  }
</style>
