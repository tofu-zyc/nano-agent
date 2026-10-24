<script lang="ts">
  import { onMount } from 'svelte';
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import { whenText } from '../../lib/format';
  import { portal } from '../../lib/portal';
  import type { ScheduleTask, ScheduleView } from '../../lib/types';
  import Icon from '../Icon.svelte';

  let view = $state<ScheduleView>({ tasks: [], timezone: '' });
  let loading = $state(true);
  let busy = $state(false);
  let editing = $state(false);
  let error = $state('');
  // 表单
  let id = $state('');
  let name = $state('');
  let when = $state('0 8 * * *');
  let prompt = $state('');
  let mode = $state('ask');
  let enabled = $state(true);
  let catchUp = $state(true);
  let timeout = $state('30m');
  let deliver = $state<string[]>([]);
  let workspaces = $state<string[]>([]);
  const modes: Record<string, string> = { readonly: '只读', ask: '审批', auto: '自动' };

  // 结果能发到哪里：网页里的会话、各个聊天窗口
  const destinations = $derived.by(() => {
    const map = new Map<string, string>();
    for (const s of store.ov?.sessions ?? []) {
      if (!s.sid.startsWith('cron:')) map.set('web:' + s.sid, '网页 · ' + s.title);
      for (const ep of s.endpoints ?? []) if (ep.channel !== 'web') map.set(ep.key, (ep.name || ep.key) + ' · ' + ep.key);
    }
    for (const key of deliver) if (!map.has(key)) map.set(key, key);
    return [...map].map(([key, label]) => ({ key, label }));
  });

  async function load() {
    try {
      view = await api.get<ScheduleView>('/api/schedules');
    } catch (err) {
      store.fail(err);
    } finally {
      loading = false;
    }
  }
  onMount(() => {
    load();
    const off = store.on((e) => {
      if (e.t === 'schedules') load();
    });
    if (store.scheduleNew) {
      store.scheduleNew = false;
      edit();
    }
    return off;
  });

  function date(s: string) {
    if (!s || s.startsWith('0001')) return '无';
    try {
      return new Intl.DateTimeFormat('zh-CN', { timeZone: view.timezone, dateStyle: 'medium', timeStyle: 'short' }).format(new Date(s));
    } catch {
      return new Date(s).toLocaleString();
    }
  }
  function edit(t?: ScheduleTask) {
    id = t?.id ?? '';
    name = t?.name ?? '';
    when = t?.when ?? '0 8 * * *';
    prompt = t?.prompt ?? '';
    mode = t?.mode ?? store.session('main')?.mode ?? 'ask';
    enabled = t?.enabled ?? true;
    catchUp = t?.catch_up ?? true;
    timeout = t?.timeout || '30m';
    deliver = [...(t?.deliver ?? ['web:main'])];
    workspaces = [...(t?.workspaces ?? store.session('main')?.workspaces.map((w) => w.id) ?? [])];
    error = '';
    editing = true;
  }
  const labelOf = (key: string) => destinations.find((d) => d.key === key)?.label ?? key;
  const toggle = (xs: string[], key: string, on: boolean) => (on ? [...new Set([...xs, key])] : xs.filter((x) => x !== key));

  async function act(action: string, t: ScheduleTask, extra: Record<string, unknown> = {}) {
    if (busy) return;
    if (action === 'delete' && !confirm('删除定时任务「' + t.name + '」？')) return;
    busy = true;
    try {
      await api.post('/api/schedules', { action, id: t.id, sid: 'main', ...extra });
      await load();
    } catch (err) {
      store.fail(err);
    } finally {
      busy = false;
    }
  }
  async function save() {
    busy = true;
    error = '';
    try {
      await api.post('/api/schedules', { action: id ? 'update' : 'create', id, sid: 'main', name, when, prompt, mode, enabled, catch_up: catchUp, timeout, deliver, workspaces });
      editing = false;
      await load();
      store.toast({ kind: 'ok', text: '定时任务已保存' });
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      busy = false;
    }
  }
  function onkey(e: KeyboardEvent) {
    if (editing && e.key === 'Escape' && !e.isComposing) {
      e.preventDefault();
      editing = false;
    }
  }
</script>

<svelte:window onkeydown={onkey} />

<div class="pnl">
  <div class="p-actions">
    <button class="btn primary" onclick={() => edit()}><Icon name="plus" size={14} />新建任务</button>
    <span class="muted p-small tz">时区 {view.timezone || '…'} · 每次运行用独立的上下文</span>
  </div>
  {#if view.error}<div class="p-err">{view.error}</div>{/if}
  {#if loading}
    <p class="p-note">正在读取…</p>
  {:else}
    {#each view.tasks as t (t.id)}
      <div class="task">
        <div class="p-row">
          <span class="dot {t.running ? 'starting' : t.enabled ? 'online' : 'disabled'}"></span>
          <strong class="p-grow p-ellipsis">{t.name}</strong>
          <span class="muted p-small p-nowrap">{t.running ? '运行中' : t.enabled ? '下次 ' + whenText(t.next) : '已暂停'}</span>
        </div>
        <div class="muted p-small"><code>{t.when}</code> · {modes[t.mode] ?? t.mode} · {t.catch_up ? '错过一小时内补跑一次' : '不补跑'} · 投递到 {t.deliver.map(labelOf).join('、')}</div>
        <p class="prompt">{t.prompt}</p>
        {#if t.last}<div class="last" class:failed={!t.last.ok}>{t.last.ok ? '上次完成' : '上次没完成'} · {date(t.last.at)}<br />{t.last.summary.slice(0, 160)}</div>{/if}
        <div class="p-actions tight">
          <button class="btn sm" disabled={busy || t.running} onclick={() => act('run_now', t)}><Icon name="play" size={12} />立即运行</button>
          <button class="btn sm" disabled={busy || t.running} onclick={() => edit(t)}>编辑</button>
          <button class="btn sm" disabled={busy || t.running} onclick={() => act('update', t, { enabled: !t.enabled })}>{t.enabled ? '暂停' : '开启'}</button>
          <a class="btn sm ghost" href={'#/sessions/' + encodeURIComponent('cron:' + t.id)} onclick={() => (store.inspector = null)}>任务会话</a>
          <button class="btn sm ghost danger" disabled={busy || t.running} onclick={() => act('delete', t)} title="删除"><Icon name="trash" size={13} /></button>
        </div>
        {#if t.results?.length}
          <details>
            <summary>最近 {t.results.length} 次结果</summary>
            {#each [...t.results].reverse() as r}
              <div class="result"><strong>{r.ok ? '完成' : '未完成'} · {date(r.at)}</strong><p>{r.summary}</p></div>
            {/each}
          </details>
        {/if}
      </div>
    {:else}
      <div class="p-card">
        <p class="p-note">还没有定时任务。在聊天里说“每天 8 点给我发 AI 新闻”，模型会帮你建；也可以点上面的「新建任务」。</p>
      </div>
    {/each}
  {/if}
</div>

{#if editing}
  <div class="backdrop" use:portal onclick={(e) => e.target === e.currentTarget && (editing = false)} role="presentation">
    <form class="sheet" onsubmit={(e) => { e.preventDefault(); save(); }}>
      <div class="top">
        <h3>{id ? '编辑任务' : '新建任务'}</h3>
        <button type="button" class="btn ghost sm" onclick={() => (editing = false)} aria-label="关闭"><Icon name="x" size={15} /></button>
      </div>
      <label class="p-label">名称<input class="p-input" bind:value={name} required placeholder="早报" /></label>
      <label class="p-label">运行时间<input class="p-input" bind:value={when} required placeholder="0 8 * * *" /></label>
      <p class="p-note p-small">每天 8 点：<code>0 8 * * *</code>；每隔半小时：<code>every 30m</code>；一次：<code>at 2026-10-24 08:00</code>。用 {view.timezone} 时区（在 Agent 节点里改）。</p>
      <label class="p-label">要做什么<textarea class="p-input area" bind:value={prompt} required rows="4" placeholder="搜索 AI 新闻，整理摘要并附上来源链接"></textarea></label>
      <div class="two">
        <label class="p-label">权限模式
          <select class="p-select" bind:value={mode}><option value="readonly">只读</option><option value="ask">审批</option><option value="auto">自动</option></select>
        </label>
        <label class="p-label">运行超时<input class="p-input" bind:value={timeout} required placeholder="30m" /></label>
      </div>
      <p class="p-note p-small">权限不能高于创建时或当前主会话的模式。需要审批时，在投递的窗口或网页里批准。</p>
      <fieldset>
        <legend>结果投递到</legend>
        {#each destinations as d}
          <label class="check"><input type="checkbox" checked={deliver.includes(d.key)} onchange={(e) => (deliver = toggle(deliver, d.key, e.currentTarget.checked))} />{d.label}</label>
        {/each}
      </fieldset>
      <fieldset>
        <legend>工作目录</legend>
        {#each store.ov?.workspaces ?? [] as w}
          <label class="check"><input type="checkbox" checked={workspaces.includes(w.id)} onchange={(e) => (workspaces = toggle(workspaces, w.id, e.currentTarget.checked))} />{w.host}:{w.path}</label>
        {:else}
          <p class="p-note p-small">还没有工作目录，任务在 HOME 下工作。</p>
        {/each}
      </fieldset>
      <div class="checks">
        <label class="check"><input type="checkbox" bind:checked={enabled} />开启任务</label>
        <label class="check"><input type="checkbox" bind:checked={catchUp} />错过一小时内补跑一次</label>
      </div>
      {#if error}<div class="p-err" role="alert">{error}</div>{/if}
      <div class="acts">
        <button type="button" class="btn" onclick={() => (editing = false)}>取消</button>
        <button class="btn primary" disabled={busy}>{busy ? '保存中…' : '保存任务'}</button>
      </div>
    </form>
  </div>
{/if}

<style>
  .tz {
    align-self: center;
  }
  .task {
    display: flex;
    flex-direction: column;
    gap: 7px;
    padding: 12px 13px;
    margin-bottom: 8px;
    border-radius: 13px;
    border: 1px solid var(--line);
    background: var(--panel-2);
  }
  .prompt {
    margin: 0;
    white-space: pre-wrap;
    font-size: 13px;
    line-height: 1.6;
    max-height: 90px;
    overflow: auto;
  }
  .last {
    padding: 8px 10px;
    background: var(--bg-2);
    border-radius: 9px;
    font-size: 12px;
    line-height: 1.6;
    white-space: pre-wrap;
  }
  .last.failed {
    color: var(--warn);
  }
  details {
    font-size: 12px;
  }
  summary {
    cursor: pointer;
    color: var(--muted);
  }
  .result {
    border-top: 1px solid var(--line);
    margin-top: 10px;
    padding-top: 10px;
  }
  .result p {
    white-space: pre-wrap;
    line-height: 1.6;
    overflow-wrap: anywhere;
    margin: 4px 0 0;
  }
  .backdrop {
    position: fixed;
    inset: 0;
    z-index: 40;
    display: grid;
    place-items: center;
    padding: 24px;
    background: color-mix(in srgb, var(--bg) 60%, transparent);
    backdrop-filter: blur(4px);
  }
  .sheet {
    width: min(640px, 100%);
    max-height: 100%;
    overflow: auto;
    display: flex;
    flex-direction: column;
    gap: 12px;
    padding: 20px 22px;
    border-radius: 18px;
    border: 1px solid var(--line-2);
    background: var(--panel-solid);
    box-shadow: 0 24px 80px rgba(0, 0, 0, 0.4);
    animation: rise 0.2s ease;
  }
  .top {
    display: flex;
    align-items: center;
    justify-content: space-between;
  }
  h3 {
    margin: 0;
    font-size: 16px;
  }
  .area {
    height: auto;
    padding: 9px 11px;
    resize: vertical;
    line-height: 1.6;
  }
  .two {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 14px;
  }
  fieldset {
    border: 1px solid var(--line);
    border-radius: 10px;
    padding: 8px 12px;
    max-height: 170px;
    overflow: auto;
    margin: 0;
  }
  legend {
    font-size: 12px;
    color: var(--muted);
  }
  .check {
    display: flex;
    align-items: center;
    gap: 7px;
    margin: 5px 0;
    font-size: 12.5px;
  }
  .checks {
    display: flex;
    flex-wrap: wrap;
    gap: 4px 18px;
  }
  .acts {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
  }
</style>
