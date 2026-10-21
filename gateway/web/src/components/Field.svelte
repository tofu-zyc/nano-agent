<script lang="ts">
  import { store } from '../lib/state.svelte';
  import type { FieldState } from '../lib/types';
  import ChipList from './ChipList.svelte';
  import Icon from './Icon.svelte';

  let { f, highlight = false }: { f: FieldState; highlight?: boolean } = $props();

  // 选项的中文名
  const optionLabels: Record<string, Record<string, string>> = {
    'workspace.mode': { auto: '自动', ask: '审批', readonly: '只读' },
    'access.guest': { none: '只能聊天', readonly: '可以读文件' },
    'llm.thinking_style': { none: '不传', vllm: 'vLLM', openai: 'OpenAI' },
    'llm.provider': { openai: 'OpenAI 兼容', mock: '假模型' },
    'feishu.domain': { feishu: '飞书', lark: 'Lark' },
  };
  const sourceLabel = { web: '网页里改过', file: '配置文件', env: '环境变量', default: '默认值' };

  const toText = (v: any) => (v === null || v === undefined ? '' : f.type === 'json' ? JSON.stringify(v, null, 2) : String(v));
  let draft = $state('');
  let reveal = $state(false);
  let saved = $state(false);
  let error = $state('');

  $effect(() => {
    draft = f.type === 'secret' ? '' : toText(f.value);
  });

  async function save(v: unknown) {
    error = '';
    const ok = await store.setConfig({ [f.key]: v }, true);
    if (ok) {
      saved = true;
      setTimeout(() => (saved = false), 1400);
    }
  }

  function commit() {
    if (f.type === 'secret') {
      if (draft.trim()) save(draft.trim()).then(() => (draft = ''));
      return;
    }
    if (draft === toText(f.value)) return;
    if (f.type === 'int' || f.type === 'float') {
      if (draft.trim() === '') return save(null);
      const n = Number(draft);
      if (Number.isNaN(n) || (f.type === 'int' && !Number.isInteger(n))) {
        error = f.type === 'int' ? '要填整数' : '要填数字';
        return;
      }
      return save(n);
    }
    if (f.type === 'json') {
      try {
        const v = draft.trim() ? JSON.parse(draft) : {};
        if (typeof v !== 'object' || Array.isArray(v) || v === null) throw new Error();
        return save(v);
      } catch {
        error = '要填 JSON 对象，比如 {"key": "value"}';
        return;
      }
    }
    save(draft);
  }

  function onkey(e: KeyboardEvent) {
    if (e.key === 'Enter' && f.type !== 'json') {
      e.preventDefault();
      (e.target as HTMLInputElement).blur();
    } else if (e.key === 'Escape') {
      draft = f.type === 'secret' ? '' : toText(f.value);
      (e.target as HTMLInputElement).blur();
    }
  }

  const labelOf = (o: string) => optionLabels[f.key]?.[o] ?? o;
</script>

<div class="field" class:hl={highlight} id={'f-' + f.key}>
  <div class="head">
    <span class="label">{f.label}</span>
    {#if saved}<span class="saved"><Icon name="check" size={12} />已保存</span>{/if}
    <span class="grow"></span>
    {#if f.restart}
      <span class="tag {f.pending ? 'warn' : ''}" title="这一项要重启网关才生效">{f.pending ? '待重启' : '重启生效'}</span>
    {/if}
    {#if f.source === 'web'}
      <button class="revert" title="去掉网页里的修改，恢复成配置文件里的值" onclick={() => save(null)}>
        <Icon name="undo" size={12} />
      </button>
    {/if}
    <span class="src {f.source}" title="当前值来自：{sourceLabel[f.source]}"></span>
  </div>

  {#if f.readonly}
    <div class="ro mono"><Icon name="lock" size={12} /> {toText(f.value) || '—'}</div>
  {:else if f.key === 'llm.thinking'}
    {@const levels = (store.field('llm.thinking_levels')?.value ?? []) as string[]}
    <div class="seg">
      <button class:active={!f.value} onclick={() => f.value && save('')}>模型默认</button>
      {#each levels as o}
        <button class:active={f.value === o} onclick={() => f.value !== o && save(o)}>{o}</button>
      {/each}
    </div>
  {:else if f.type === 'bool'}
    <button
      class="switch"
      class:on={!!f.value}
      role="switch"
      aria-checked={!!f.value}
      aria-label={f.label}
      onclick={() => save(!f.value)}
    ><i></i></button>
  {:else if f.type === 'enum'}
    {#if (f.options?.length ?? 0) <= 4}
      <div class="seg">
        {#each f.options ?? [] as o}
          <button class:active={f.value === o} onclick={() => f.value !== o && save(o)}>{labelOf(o)}</button>
        {/each}
      </div>
    {:else}
      <select class="input" value={f.value} onchange={(e) => save((e.target as HTMLSelectElement).value)}>
        {#each f.options ?? [] as o}<option value={o}>{labelOf(o)}</option>{/each}
      </select>
    {/if}
  {:else if f.type === 'list'}
    <ChipList values={f.value ?? []} onchange={(v) => save(v)} mono={f.key.includes('dir') || f.key.includes('_r')} />
  {:else if f.type === 'json'}
    <textarea class="input mono" rows="4" bind:value={draft} onblur={commit} onkeydown={onkey} spellcheck="false"></textarea>
  {:else if f.type === 'secret'}
    <div class="secret">
      <input
        class="input"
        type={reveal ? 'text' : 'password'}
        bind:value={draft}
        onblur={commit}
        onkeydown={onkey}
        placeholder={f.is_set ? '已设置 · 输入新值替换' : '未设置'}
        autocomplete="off"
      />
      <button class="eye" onclick={() => (reveal = !reveal)} aria-label="显示或隐藏"><Icon name={reveal ? 'eyeoff' : 'eye'} size={14} /></button>
      {#if f.is_set}<span class="set"><Icon name="key" size={12} /></span>{/if}
    </div>
  {:else}
    <input
      class="input"
      class:mono={f.key.includes('url') || f.key.includes('listen') || f.key.includes('dir') || f.key.includes('proxy')}
      type={f.type === 'int' || f.type === 'float' ? 'number' : 'text'}
      step={f.type === 'float' ? 'any' : '1'}
      bind:value={draft}
      onblur={commit}
      onkeydown={onkey}
      spellcheck="false"
    />
  {/if}

  {#if error}<p class="error">{error}</p>{/if}
  {#if f.help}<p class="help">{f.help}</p>{/if}
</div>

<style>
  .field {
    padding: 12px 14px;
    border-radius: 12px;
    transition: background 0.3s, box-shadow 0.3s;
  }
  .field.hl {
    background: color-mix(in srgb, var(--accent) 10%, transparent);
    box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--accent) 45%, transparent);
  }
  .head {
    display: flex;
    align-items: center;
    gap: 7px;
    margin-bottom: 7px;
  }
  .label {
    font-weight: 600;
    font-size: 13px;
  }
  .grow {
    flex: 1;
  }
  .saved {
    display: inline-flex;
    align-items: center;
    gap: 3px;
    color: var(--ok);
    font-size: 11.5px;
    animation: rise 0.2s ease;
  }
  .revert {
    display: grid;
    place-items: center;
    width: 22px;
    height: 22px;
    border-radius: 6px;
    border: 1px solid var(--line-2);
    background: var(--panel-2);
    color: var(--text-2);
  }
  .revert:hover {
    color: var(--accent-2);
    border-color: var(--accent);
  }
  .src {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: var(--line-2);
  }
  .src.web {
    background: var(--accent);
  }
  .src.file {
    background: var(--text-2);
  }
  .src.env {
    background: var(--c-model);
  }
  .help {
    margin: 7px 0 0;
    font-size: 12px;
    line-height: 1.5;
    color: var(--muted);
  }
  .error {
    margin: 6px 0 0;
    font-size: 12px;
    color: var(--err);
  }
  .ro {
    display: flex;
    align-items: center;
    gap: 6px;
    color: var(--text-2);
    word-break: break-all;
  }
  .switch {
    position: relative;
    width: 40px;
    height: 23px;
    border-radius: 12px;
    border: 1px solid var(--line-2);
    background: var(--bg-2);
    transition: background 0.2s, border-color 0.2s;
  }
  .switch i {
    position: absolute;
    top: 2px;
    left: 2px;
    width: 17px;
    height: 17px;
    border-radius: 50%;
    background: var(--muted);
    transition: transform 0.2s, background 0.2s;
  }
  .switch.on {
    background: var(--accent);
    border-color: transparent;
  }
  .switch.on i {
    transform: translateX(17px);
    background: #fff;
  }
  .seg {
    display: inline-flex;
    padding: 3px;
    gap: 2px;
    border-radius: 10px;
    background: var(--bg-2);
    border: 1px solid var(--line);
  }
  .seg button {
    height: 28px;
    padding: 0 13px;
    border: none;
    border-radius: 7px;
    background: none;
    color: var(--text-2);
    font-size: 12.5px;
    transition: background 0.15s, color 0.15s;
  }
  .seg button:hover {
    color: var(--text);
  }
  .seg button.active {
    background: var(--panel-solid);
    color: var(--text);
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2), inset 0 0 0 1px var(--line-2);
  }
  .secret {
    position: relative;
  }
  .secret .input {
    padding-right: 60px;
  }
  .eye {
    position: absolute;
    right: 6px;
    top: 6px;
    width: 24px;
    height: 22px;
    border: none;
    background: none;
    color: var(--muted);
  }
  .set {
    position: absolute;
    right: 34px;
    top: 9px;
    color: var(--ok);
  }
  textarea {
    font-size: 12px;
  }
</style>
