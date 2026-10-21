<script lang="ts">
  import Icon from './Icon.svelte';

  let {
    values,
    onchange,
    placeholder = '输入后回车添加',
    mono = false,
    disabled = false,
  }: { values: string[]; onchange: (v: string[]) => void; placeholder?: string; mono?: boolean; disabled?: boolean } = $props();

  let text = $state('');

  function add() {
    const v = text.trim();
    if (!v) return;
    text = '';
    if (!values.includes(v)) onchange([...values, v]);
  }

  function remove(i: number) {
    onchange(values.filter((_, j) => j !== i));
  }

  function onkey(e: KeyboardEvent) {
    if (e.key === 'Enter') {
      e.preventDefault();
      add();
    } else if (e.key === 'Backspace' && !text && values.length) {
      remove(values.length - 1);
    }
  }
</script>

<div class="chips" class:disabled>
  {#each values as v, i}
    <span class="chip" class:mono title={v}>
      {#if i === 0 && mono}<i class="first" title="第一个是默认的"></i>{/if}
      <span class="v">{v}</span>
      {#if !disabled}
        <button onclick={() => remove(i)} aria-label="移除"><Icon name="x" size={11} /></button>
      {/if}
    </span>
  {/each}
  {#if !disabled}
    <input bind:value={text} onkeydown={onkey} onblur={add} {placeholder} class:mono />
  {/if}
</div>

<style>
  .chips {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    padding: 6px;
    min-height: 36px;
    border-radius: 9px;
    background: var(--bg-2);
    border: 1px solid var(--line);
    transition: border-color 0.15s;
  }
  .chips:focus-within {
    border-color: var(--accent);
  }
  .chip {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    max-width: 100%;
    height: 24px;
    padding: 0 4px 0 9px;
    border-radius: 7px;
    background: var(--panel-2);
    border: 1px solid var(--line-2);
    font-size: 12.5px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .v {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .chip.mono {
    font-family: var(--mono);
    font-size: 12px;
  }
  .first {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--c-kernel);
  }
  .chip button {
    display: grid;
    place-items: center;
    width: 18px;
    height: 18px;
    border: none;
    border-radius: 5px;
    background: none;
    color: var(--muted);
  }
  .chip button:hover {
    background: color-mix(in srgb, var(--err) 18%, transparent);
    color: var(--err);
  }
  input {
    flex: 1;
    min-width: 120px;
    border: none;
    outline: none;
    background: none;
    font-size: 13px;
    padding: 0 4px;
  }
  input.mono {
    font-family: var(--mono);
    font-size: 12px;
  }
  .disabled {
    opacity: 0.7;
  }
</style>
