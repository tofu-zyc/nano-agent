<script lang="ts">
  import { onMount } from 'svelte';
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import { channelMeta, whenText } from '../../lib/format';
  import { buildWhen, describeWhen, parseWhen, weekdays, type WhenForm } from '../../lib/when';
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
  let when = $state<WhenForm>(parseWhen('0 8 * * *'));
  let prompt = $state('');
  let mode = $state('ask');
  let enabled = $state(true);
  let catchUp = $state(true);
  let timeout = $state('30m');
  let deliver = $state<string[]>([]);
  let workspaces = $state<string[]>([]);
  let agent = $state('default'); // 这个任务由哪个 agent 来跑
  const modes: Record<string, string> = { readonly: '只读', ask: '审批', auto: '自动' };

  // 结果能发到哪里：聊天窗口（飞书、Telegram、QQ、微信、命令行），和普通的网页会话。
  // 定时任务、内心活动、做梦、网关里的群这些会话不列：发过去没人看（别的 agent 的会话 ID 前面带 @<agent>/）
  const hiddenSid = (sid: string) => /(^|\/)(cron|room):/.test(sid) || /^@[^/]+\/(life|dream)$/.test(sid);
  const destinations = $derived.by(() => {
    const chats = new Map<string, string>();
    const webs = new Map<string, string>();
    for (const s of store.ov?.sessions ?? []) {
      if (!hiddenSid(s.sid) && !s.group) webs.set('web:' + s.sid, s.title || s.sid);
      for (const ep of s.endpoints ?? []) {
        if (ep.channel !== 'web') chats.set(ep.key, (channelMeta[ep.channel]?.label ?? ep.channel) + ' · ' + (ep.name || ep.key));
      }
    }
    // 任务里已经选了、但现在列表里没有的（窗口不在了），也列出来，好取消
    for (const key of deliver) if (!chats.has(key) && !webs.has(key)) (key.startsWith('web:') ? webs : chats).set(key, key);
    const list = (m: Map<string, string>) => [...m].map(([key, label]) => ({ key, label }));
    return { chats: list(chats), webs: list(webs) };
  });
  const whenBuilt = $derived(buildWhen(when));

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
    when = parseWhen(t?.when ?? '0 8 * * *');
    prompt = t?.prompt ?? '';
    mode = t?.mode ?? store.session('main')?.mode ?? 'ask';
    enabled = t?.enabled ?? true;
    catchUp = t?.catch_up ?? true;
    timeout = t?.timeout || '30m';
    deliver = [...(t?.deliver ?? [])]; // 新任务默认只放任务页，想推到哪个窗口再勾
    workspaces = [...(t?.workspaces ?? store.session('main')?.workspaces.map((w) => w.id) ?? [])];
    agent = t?.agent || 'default';
    error = '';
    editing = true;
  }
  // 任务会话的 ID：默认 agent 是 cron:<id>，别的 agent 是 @<agent>/cron:<id>
  const cronSid = (t: ScheduleTask) => (t.agent && t.agent !== 'default' ? '@' + t.agent + '/cron:' + t.id : 'cron:' + t.id);
  const labelOf = (key: string) =>
    destinations.chats.find((d) => d.key === key)?.label ?? ('网页 · ' + (destinations.webs.find((d) => d.key === key)?.label ?? key));
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
    if (whenBuilt.error) {
      error = '运行时间：' + whenBuilt.error;
      return;
    }
    busy = true;
    error = '';
    try {
      await api.post('/api/schedules', {
        action: id ? 'update' : 'create', id, sid: 'main', name, when: whenBuilt.when, prompt, mode, agent, enabled, catch_up: catchUp, timeout, deliver, workspaces,
      });
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
        <div class="muted p-small">{describeWhen(parseWhen(t.when))}（<code>{t.when}</code>）· {modes[t.mode] ?? t.mode} · 由 {store.agent(t.agent)?.name ?? t.agent} 执行 · {t.catch_up ? '错过一小时内补跑一次' : '不补跑'} · {t.deliver.length ? '投递到 ' + t.deliver.map(labelOf).join('、') : '只放任务页'}</div>
        <p class="prompt">{t.prompt}</p>
        {#if t.last}<div class="last" class:failed={!t.last.ok}>{t.last.ok ? '上次完成' : '上次没完成'} · {date(t.last.at)}<br />{t.last.summary.slice(0, 160)}</div>{/if}
        <div class="p-actions tight">
          <button class="btn sm" disabled={busy || t.running} onclick={() => act('run_now', t)}><Icon name="play" size={12} />立即运行</button>
          <button class="btn sm" disabled={busy || t.running} onclick={() => edit(t)}>编辑</button>
          <button class="btn sm" disabled={busy || t.running} onclick={() => act('update', t, { enabled: !t.enabled })}>{t.enabled ? '暂停' : '开启'}</button>
          <a class="btn sm ghost" href={'#/sessions/' + encodeURIComponent(cronSid(t))} onclick={() => (store.inspector = null)}>任务会话</a>
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
      <div class="p-label">运行时间
        <div class="seg">
          {#each [['every', '每隔'], ['daily', '每天'], ['weekly', '每周'], ['once', '只运行一次'], ['custom', '自定义']] as [k, label] (k)}
            <button type="button" class:active={when.kind === k} onclick={() => (when.kind = k as WhenForm['kind'])}>{label}</button>
          {/each}
        </div>
        {#if when.kind === 'every'}
          <div class="row">
            <input class="p-input num" type="number" min="1" step="1" bind:value={when.n} aria-label="间隔" />
            <select class="p-select unit" bind:value={when.unit} aria-label="单位"><option value="m">分钟</option><option value="h">小时</option></select>
          </div>
        {:else if when.kind === 'daily'}
          <div class="row"><input class="p-input time" type="time" bind:value={when.time} aria-label="几点" /></div>
        {:else if when.kind === 'weekly'}
          <div class="row">
            <div class="days">
              {#each weekdays as w (w.d)}
                <button type="button" class:on={when.days.includes(w.d)} onclick={() => (when.days = when.days.includes(w.d) ? when.days.filter((x) => x !== w.d) : [...when.days, w.d])}>{w.label}</button>
              {/each}
            </div>
            <input class="p-input time" type="time" bind:value={when.time} aria-label="几点" />
          </div>
        {:else if when.kind === 'once'}
          <div class="row"><input class="p-input stamp" type="datetime-local" bind:value={when.at} aria-label="运行时间" /></div>
        {:else}
          <input class="p-input" bind:value={when.custom} placeholder="0 8 1 * *" aria-label="自定义运行时间" />
        {/if}
      </div>
      <p class="p-note p-small">
        {#if whenBuilt.error}{whenBuilt.error}{:else}{describeWhen(when)}（<code>{whenBuilt.when}</code>）{/if}。用 {view.timezone} 时区（在 Agent 默认值节点里改）。
        {#if when.kind === 'custom'}写法：cron 五段式（每月 1 号 8 点：<code>0 8 1 * *</code>）、<code>every 90s</code>、<code>at 2026-10-24 08:00</code>。{/if}
        {#if when.kind === 'every' && when.unit === 'm' && when.n < 10}间隔很短：每次运行都要调用模型，注意用量。{/if}
      </p>
      <label class="p-label">要做什么<textarea class="p-input area" bind:value={prompt} required rows="4" placeholder="搜索 AI 新闻，整理摘要并附上来源链接"></textarea></label>
      <div class="two">
        <label class="p-label">由谁执行
          <select class="p-select" bind:value={agent}>
            {#each store.agents as a (a.id)}<option value={a.id}>{a.emoji} {a.name}</option>{/each}
          </select>
        </label>
        <label class="p-label">权限模式
          <select class="p-select" bind:value={mode}><option value="readonly">只读</option><option value="ask">审批</option><option value="auto">自动</option></select>
        </label>
        <label class="p-label">运行超时<input class="p-input" bind:value={timeout} required placeholder="30m" /></label>
      </div>
      <p class="p-note p-small">权限不能高于创建时或当前主会话的模式。需要审批时，在投递的窗口或网页里批准。</p>
      <fieldset>
        <legend>结果推送到</legend>
        <p class="p-note p-small">不选就只记在任务页，不往聊天里推。模型回 [不回复] 的那次也不推。</p>
        <div class="opts">
          {#each destinations.chats as d (d.key)}
            <label class="check"><input type="checkbox" checked={deliver.includes(d.key)} onchange={(e) => (deliver = toggle(deliver, d.key, e.currentTarget.checked))} /><span>{d.label}</span></label>
          {/each}
          {#if destinations.webs.length}
            <div class="sub">网页会话（只在网页上显示一下，不进聊天记录）</div>
            {#each destinations.webs as d (d.key)}
              <label class="check"><input type="checkbox" checked={deliver.includes(d.key)} onchange={(e) => (deliver = toggle(deliver, d.key, e.currentTarget.checked))} /><span>{d.label}</span></label>
            {/each}
          {/if}
        </div>
      </fieldset>
      <fieldset>
        <legend>工作目录</legend>
        <div class="opts">
          {#each store.ov?.workspaces ?? [] as w (w.id)}
            <label class="check"><input type="checkbox" checked={workspaces.includes(w.id)} onchange={(e) => (workspaces = toggle(workspaces, w.id, e.currentTarget.checked))} /><span>{w.host}:{w.path}</span></label>
          {:else}
            <p class="p-note p-small">还没有工作目录，任务在 HOME 下工作。</p>
          {/each}
        </div>
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
    box-sizing: border-box;
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
  /* 弹窗是限高的纵向 flex：子元素默认会被压扁（分组框里的选项就溢出来叠在一起），一律不压，整个弹窗滚动 */
  .sheet > :global(*) {
    flex-shrink: 0;
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
    padding: 6px 12px 8px;
    margin: 0;
    min-width: 0;
  }
  fieldset .p-note {
    margin: 2px 0 4px;
  }
  /* 选项多的时候在框里滚动，别撑开整个弹窗 */
  .opts {
    max-height: 170px;
    overflow-y: auto;
  }
  .sub {
    margin: 8px 0 2px;
    font-size: 11.5px;
    color: var(--muted);
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
    min-width: 0;
  }
  .check input {
    flex: none;
    margin: 0;
  }
  .check span {
    min-width: 0;
    overflow-wrap: anywhere;
  }
  .seg {
    display: flex;
    flex-wrap: wrap;
    width: fit-content;
    max-width: 100%;
    padding: 3px;
    gap: 2px;
    margin-top: 6px;
    border-radius: 10px;
    background: var(--bg-2);
    border: 1px solid var(--line);
  }
  .seg button {
    height: 28px;
    padding: 0 12px;
    border: none;
    border-radius: 7px;
    background: none;
    color: var(--text-2);
    font-size: 12.5px;
  }
  .seg button:hover {
    color: var(--text);
  }
  .seg button.active {
    background: var(--panel-solid);
    color: var(--text);
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2), inset 0 0 0 1px var(--line-2);
  }
  .row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 10px;
    margin-top: 8px;
  }
  .num {
    width: 90px;
  }
  .unit {
    width: 100px;
  }
  .time {
    width: 130px;
  }
  .stamp {
    width: 220px;
  }
  .days {
    display: flex;
    gap: 4px;
  }
  .days button {
    width: 32px;
    height: 32px;
    border-radius: 50%;
    border: 1px solid var(--line-2);
    background: var(--bg-2);
    color: var(--text-2);
    font-size: 12.5px;
  }
  .days button.on {
    background: var(--accent);
    border-color: var(--accent);
    color: #fff;
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
  @media (max-width: 560px) {
    .backdrop {
      padding: 10px;
    }
    .sheet {
      padding: 16px;
    }
    .two {
      grid-template-columns: 1fr;
    }
  }
</style>
