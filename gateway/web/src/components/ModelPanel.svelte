<script lang="ts">
  import { store } from '../lib/state.svelte';
  import { api } from '../lib/api';
  import { num } from '../lib/format';
  import type { ModelEndpoint, ModelItem, RemoteModel } from '../lib/types';
  import Icon from './Icon.svelte';

  // 可以用的模型：配置文件里的 [llm] 是内置接口（只能在设置里改），网页里还能加别的 OpenAI 兼容接口，
  // 每个接口上可以有好几个模型；一个是默认模型，每个会话也可以单独选（会话设置、/model）。

  const mv = $derived(store.ov?.models);
  const eps = $derived(mv?.endpoints ?? []);
  const models = $derived(mv?.models ?? []);
  const def = $derived(models.find((m) => m.default));

  let busy = $state('');
  let err = $state('');
  let testing = $state('');
  let testResult = $state<{ id: string; label: string; ok: boolean; reply?: string; error?: string; ms?: number; first_ms?: number } | null>(null);

  // 接口表单
  let epOpen = $state(false);
  let epID = $state('');
  let epName = $state('');
  let epKind = $state('openai');
  let epBase = $state('');
  let epKey = $state('');
  let epProxy = $state('');

  // 模型表单
  let mOpen = $state(false);
  let mEndpoint = $state('');
  let mName = $state('');
  let mLabel = $state('');
  let mCtx = $state('');
  let mMax = $state('');
  let mHist = $state('');
  let mThink = $state('');
  let mLevels = $state('');
  let mImage = $state(false); // 这个模型能不能接收图片
  let mEditing = $state(false); // 改已有的模型（接口不能换）
  let afterEndpoint = $state(false); // 先加接口，存好以后接着加模型
  const writable = $derived(eps.filter((e) => !e.config));

  // 从命令面板、设置页过来时直接打开添加模型
  let seenAdd = 0;
  $effect(() => {
    const tick = store.addModelTick;
    if (tick && tick !== seenAdd) {
      seenAdd = tick;
      addModelFlow();
    }
  });
  let remote = $state<RemoteModel[] | null>(null);
  let remoteErr = $state('');

  function modelsOn(id: string): ModelItem[] {
    return models.filter((m) => m.endpoint === id);
  }

  function newEndpoint() {
    epID = '';
    epName = '';
    epKind = 'openai';
    epBase = '';
    epKey = '';
    epProxy = '';
    err = '';
    epOpen = true;
    mOpen = false;
  }

  function editEndpoint(e: ModelEndpoint) {
    epID = e.id;
    epName = e.name;
    epKind = e.kind;
    epBase = e.base_url;
    epKey = '';
    epProxy = e.proxy ?? '';
    err = '';
    epOpen = true;
    mOpen = false;
  }

  const fail = (e: unknown) => (e instanceof Error ? e.message : String(e));

  async function saveEndpoint() {
    if (!epName.trim()) return;
    busy = 'endpoint';
    err = '';
    const body = { name: epName.trim(), kind: epKind, base_url: epBase.trim(), api_key: epKey, proxy: epProxy.trim() };
    try {
      const r = epID
        ? await api.put<{ id: string }>('/api/models/endpoints/' + encodeURIComponent(epID), body)
        : await api.post<{ id: string }>('/api/models/endpoints', body);
      epOpen = false;
      await store.reloadModels();
      const next = afterEndpoint;
      afterEndpoint = false;
      if (next) newModel(r.id);
      store.toast({ kind: 'ok', text: '接口已保存' }, 1800);
    } catch (e) {
      err = fail(e);
    } finally {
      busy = '';
    }
  }

  async function removeEndpoint(e: ModelEndpoint) {
    const n = modelsOn(e.id).length;
    if (!confirm(`删除接口「${e.name}」？${n ? `它上面的 ${n} 个模型也会删掉，` : ''}机器上的服务不会受影响。`)) return;
    try {
      await api.del('/api/models/endpoints/' + encodeURIComponent(e.id));
      await store.reloadModels();
      store.toast({ kind: 'ok', text: '接口已删除' }, 1800);
    } catch (e) {
      store.fail(e);
    }
  }

  // 顶部的「添加模型」：还没有接口就先去加接口，加好以后自动接着加模型
  function addModelFlow() {
    if (!writable.length) {
      afterEndpoint = true;
      newEndpoint();
      return;
    }
    newModel(writable[0].id);
  }

  function newModel(endpoint: string) {
    mEndpoint = endpoint;
    mName = '';
    mLabel = '';
    mCtx = '';
    mMax = '';
    mHist = '';
    mThink = '';
    mLevels = '';
    mImage = false;
    mEditing = false;
    afterEndpoint = false;
    remote = null;
    remoteErr = '';
    err = '';
    mOpen = true;
    epOpen = false;
  }

  function editModel(m: ModelItem) {
    mEndpoint = m.endpoint;
    mName = m.name;
    mLabel = m.label === m.name ? '' : m.label;
    mCtx = m.context_window ? String(m.context_window) : '';
    mMax = m.max_tokens ? String(m.max_tokens) : '';
    mHist = m.max_history_tokens ? String(m.max_history_tokens) : '';
    mThink = m.thinking;
    mLevels = (m.thinking_levels ?? []).join(', ');
    mImage = (m.modalities ?? []).includes('image');
    mEditing = true;
    remote = null;
    remoteErr = '';
    err = '';
    mOpen = true;
    epOpen = false;
  }

  const intOf = (s: string) => {
    const n = parseInt(s, 10);
    return Number.isFinite(n) && n > 0 ? n : 0;
  };

  async function saveModel() {
    if (!mName.trim()) return;
    busy = 'model';
    err = '';
    try {
      await api.post('/api/models/items', {
        endpoint: mEndpoint,
        name: mName.trim(),
        label: mLabel.trim(),
        context_window: intOf(mCtx),
        max_tokens: intOf(mMax),
        max_history_tokens: intOf(mHist),
        thinking: mThink.trim(),
        modalities: mImage ? ['text', 'image'] : ['text'],
        thinking_levels: mLevels
          .split(/[,，\s]+/)
          .map((s) => s.trim())
          .filter(Boolean),
      });
      mOpen = false;
      mEditing = false;
      remote = null;
      await store.reloadModels();
      store.toast({ kind: 'ok', text: '模型已保存' }, 1800);
    } catch (e) {
      err = fail(e);
    } finally {
      busy = '';
    }
  }

  async function removeModel(m: ModelItem) {
    if (!confirm(`删除模型「${m.label}」？单独选了它的会话会改回默认模型。`)) return;
    try {
      await api.del('/api/models/items?id=' + encodeURIComponent(m.id));
      await store.reloadModels();
      store.toast({ kind: 'ok', text: '模型已删除' }, 1800);
    } catch (e) {
      store.fail(e);
    }
  }

  async function setDefault(m: ModelItem) {
    if (m.default) return;
    try {
      await api.post('/api/models/default', { id: m.id });
      await store.reloadModels();
      store.toast({ kind: 'ok', text: `新会话默认用 ${m.label}` }, 2400);
    } catch (e) {
      store.fail(e);
    }
  }

  async function test(id: string, label: string) {
    if (testing) return;
    testing = id;
    testResult = null;
    try {
      const r = await api.post<{ ok: boolean; reply?: string; error?: string; ms?: number; first_ms?: number }>('/api/test/model', { id });
      testResult = { id, label, ok: !!r.ok, reply: r.reply, error: r.error, ms: r.ms, first_ms: r.first_ms };
    } catch (e) {
      testResult = { id, label, ok: false, error: fail(e) };
    } finally {
      testing = '';
    }
  }

  async function fetchRemote() {
    if (!mEndpoint) return;
    busy = 'fetch';
    remoteErr = '';
    try {
      const r = await api.post<{ models: RemoteModel[] }>('/api/models/fetch', { endpoint: mEndpoint });
      remote = r.models ?? [];
    } catch (e) {
      remoteErr = fail(e);
    } finally {
      busy = '';
    }
  }

  function pickRemote(r: RemoteModel) {
    mName = r.id;
    if (r.context_window) mCtx = String(r.context_window);
    if (r.modalities?.includes('image')) mImage = true;
    remote = null;
  }

  const epName0 = (id: string) => eps.find((e) => e.id === id)?.name ?? id;
</script>

<h5>默认模型 <span class="muted small">新会话、没单独选模型的会话都用它</span></h5>
<div class="card">
  {#if def}
    <div class="row">
      <span class="dot online"></span>
      <strong class="grow">{def.label}</strong>
      <span class="tag accent">默认</span>
    </div>
    <div class="muted small">
      <code>{def.id}</code>
      {#if def.context_window} · 上下文 {num(def.context_window)}{/if}
      {#if def.thinking} · 思考 {def.thinking}{/if}
    </div>
    <div class="row">
      <button class="btn sm" disabled={!!testing} onclick={() => test(def.id, def.label)}>
        <Icon name={testing === def.id ? 'refresh' : 'zap'} size={13} class={testing === def.id ? 'spin' : ''} />
        {testing === def.id ? '正在测试…' : '测试连接'}
      </button>
    </div>
    {#if testResult?.id === def.id}
      <div class="result" class:bad={!testResult.ok}>
        {#if testResult.ok}
          <div class="rt"><Icon name="check" size={13} />通了 · 首字 {testResult.first_ms ?? '—'} ms · 总共 {testResult.ms ?? '—'} ms</div>
          <p>{testResult.reply}</p>
        {:else}
          <div class="rt"><Icon name="alert" size={13} />没通</div>
          <p>{testResult.error}</p>
        {/if}
      </div>
    {/if}
  {:else}
    <p class="note">还没有可用的模型。下面添加一个接口和模型。</p>
  {/if}
</div>

<div class="acts">
  <button class="btn primary" onclick={addModelFlow} disabled={epOpen || mOpen}>
    <Icon name="plus" size={14} />添加模型
  </button>
  <button class="btn" onclick={newEndpoint} disabled={epOpen || mOpen}>
    <Icon name="plus" size={14} />添加接口
  </button>
  {#if !writable.length}<span class="muted small">先加一个接口，再给它添加模型</span>{/if}
</div>

{#if epOpen}
  <div class="card form">
    <div class="row">
      <strong class="grow">{epID ? '编辑接口' : '添加接口'}</strong>
      <button class="btn sm ghost" aria-label="关闭" onclick={() => (epOpen = false)}><Icon name="x" size={13} /></button>
    </div>
    {#if afterEndpoint}<p class="note small">保存后接着给这个接口添加模型。</p>{/if}
    <label>名字<input class="input" bind:value={epName} placeholder="比如 本地 vLLM" /></label>
    <label>类型
      <select class="input" bind:value={epKind}>
        <option value="openai">openai（兼容接口）</option>
        <option value="mock">mock（假模型，调试用）</option>
      </select>
    </label>
    {#if epKind === 'openai'}
      <label>接口地址<input class="input" bind:value={epBase} placeholder="https://api.deepseek.com/v1" /></label>
    {/if}
    <label>API Key
      <input class="input" type="password" bind:value={epKey} autocomplete="new-password"
        placeholder={epID ? '留空表示不改' : '内网接口可以留空'} />
    </label>
    <label>代理<input class="input" bind:value={epProxy} placeholder="留空用网关的默认代理；none 表示直连" /></label>
    {#if err}<div class="errbox">{err}</div>{/if}
    <div class="row">
      <span class="grow"></span>
      <button class="btn sm" onclick={() => (epOpen = false)}>取消</button>
      <button class="btn sm primary" disabled={busy === 'endpoint' || !epName.trim()} onclick={saveEndpoint}>
        {busy === 'endpoint' ? '正在保存…' : '保存'}
      </button>
    </div>
  </div>
{/if}

{#if mOpen}
  <div class="card form">
    <div class="row">
      <strong class="grow">{mEditing ? '编辑模型' : '添加模型'} <span class="muted small">· {epName0(mEndpoint)}</span></strong>
      <button class="btn sm ghost" aria-label="关闭" onclick={() => (mOpen = false)}><Icon name="x" size={13} /></button>
    </div>
    {#if mEditing}
      <div class="muted small">接口：{epName0(mEndpoint)}</div>
    {:else}
      <label>接口
        <select class="input" bind:value={mEndpoint}>
          {#each writable as e (e.id)}<option value={e.id}>{e.name}</option>{/each}
        </select>
      </label>
    {/if}
    <div class="row">
      <button class="btn sm" disabled={busy === 'fetch'} onclick={fetchRemote}>
        <Icon name={busy === 'fetch' ? 'refresh' : 'down'} size={13} class={busy === 'fetch' ? 'spin' : ''} />
        {busy === 'fetch' ? '正在读取…' : '从接口读取模型列表'}
      </button>
      <span class="muted small">或者手动填</span>
    </div>
    {#if remoteErr}<div class="errbox">{remoteErr}</div>{/if}
    {#if remote}
      <div class="remote">
        {#each remote as r (r.id)}
          <button class="rrow" onclick={() => pickRemote(r)}>
            <span class="grow"><code>{r.id}</code>{#if r.added}<span class="tag">已添加</span>{/if}</span>
            {#if r.context_window}<span class="muted small">{num(r.context_window)}</span>{/if}
          </button>
        {:else}
          <p class="note small">接口没有返回模型。</p>
        {/each}
      </div>
    {/if}
    <label>模型名<input class="input" bind:value={mName} placeholder="接口里的模型名，比如 deepseek-chat" /></label>
    <label>显示名<input class="input" bind:value={mLabel} placeholder="网页和 /model 里显示的名字，留空就用模型名" /></label>
    <div class="two">
      <label>上下文窗口<input class="input" bind:value={mCtx} inputmode="numeric" placeholder="比如 131072" /></label>
      <label>最大输出<input class="input" bind:value={mMax} inputmode="numeric" placeholder="留空用配置" /></label>
    </div>
    <label>历史上限<input class="input" bind:value={mHist} inputmode="numeric" placeholder="留空按上下文窗口的一半" /></label>
    <div class="two">
      <label>默认思考强度<input class="input" bind:value={mThink} placeholder="比如 high，留空不传" /></label>
      <label>可选值<input class="input" bind:value={mLevels} placeholder="none, low, high" /></label>
    </div>
    <label class="check"><input type="checkbox" bind:checked={mImage} /> 能接收图片（多模态模型）</label>
    {#if err}<div class="errbox">{err}</div>{/if}
    <div class="row">
      <span class="grow"></span>
      <button class="btn sm" onclick={() => (mOpen = false)}>取消</button>
      <button class="btn sm primary" disabled={busy === 'model' || !mName.trim()} onclick={saveModel}>
        {busy === 'model' ? '正在保存…' : '保存'}
      </button>
    </div>
  </div>
{/if}

<h5>接口 <span class="muted small">网页里加的模型存在数据目录的 models.json</span></h5>
{#each eps as e (e.id)}
  <div class="card">
    <div class="row">
      <Icon name={e.kind === 'mock' ? 'bot' : 'sparkles'} size={15} />
      <strong class="grow">{e.name}</strong>
      {#if e.config}
        <span class="tag">配置文件</span>
      {:else}
        <span class="tag">{e.kind}</span>
        <button class="btn sm ghost" title="编辑接口" onclick={() => editEndpoint(e)}><Icon name="edit" size={13} /></button>
        <button class="btn sm ghost" title="删除接口" onclick={() => removeEndpoint(e)}><Icon name="trash" size={13} /></button>
      {/if}
    </div>
    <div class="muted small mono base">{e.base_url || '—'}</div>
    <div class="row meta">
      <span class="muted small"><Icon name="key" size={12} />{e.has_key ? '已设置密钥' : '没有密钥'}</span>
      {#if e.proxy}<span class="muted small">代理 {e.proxy}</span>{/if}
    </div>

    {#each modelsOn(e.id) as m (m.id)}
      <div class="mrow">
        <button class="star" class:on={m.default} title={m.default ? '新会话默认用它' : '设为新会话的默认模型'} onclick={() => setDefault(m)}>
          {m.default ? '★' : '☆'}
        </button>
        <div class="grow min">
          <div class="mt">
            <strong>{m.label}</strong>
            {#if m.modalities?.includes('image')}<span class="tag accent">图片</span>{/if}
            {#if m.config}<span class="tag">配置文件</span>{/if}
          </div>
          <div class="muted small msub">
            <code>{m.name}</code>
            {#if m.context_window} · 上下文 {num(m.context_window)}{/if}
            {#if m.thinking_levels?.length} · 思考 {m.thinking_levels.join('/')}{/if}
          </div>
        </div>
        <button class="btn sm ghost" disabled={!!testing} title="测试连接" onclick={() => test(m.id, m.label)}>
          <Icon name={testing === m.id ? 'refresh' : 'zap'} size={13} class={testing === m.id ? 'spin' : ''} />
        </button>
        {#if !m.config}
          <button class="btn sm ghost" title="编辑模型" onclick={() => editModel(m)}><Icon name="edit" size={13} /></button>
          <button class="btn sm ghost" title="删除模型" onclick={() => removeModel(m)}><Icon name="trash" size={13} /></button>
        {/if}
      </div>
      {#if testResult?.id === m.id}
        <div class="result" class:bad={!testResult.ok}>
          {#if testResult.ok}
            <div class="rt"><Icon name="check" size={13} />通了 · 首字 {testResult.first_ms ?? '—'} ms · 总共 {testResult.ms ?? '—'} ms</div>
            <p>{testResult.reply}</p>
          {:else}
            <div class="rt"><Icon name="alert" size={13} />没通</div>
            <p>{testResult.error}</p>
          {/if}
        </div>
      {/if}
    {:else}
      <p class="note small">这个接口上还没有模型。</p>
    {/each}
    {#if !e.config}
      <button class="btn sm" disabled={mOpen || epOpen} onclick={() => newModel(e.id)}><Icon name="plus" size={13} />添加模型</button>
    {/if}
  </div>
{:else}
  <p class="note">还没有接口。配置文件里的接口在下面的设置里改。</p>
{/each}

<button class="btn sm" disabled={epOpen || mOpen} onclick={newEndpoint}><Icon name="plus" size={13} />添加接口</button>

<style>
  h5 {
    margin: 18px 0 8px 2px;
    font-size: 12px;
    font-weight: 600;
    letter-spacing: 0.5px;
    color: var(--muted);
  }
  .card {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 11px 12px;
    margin-bottom: 8px;
    border-radius: 12px;
    background: var(--panel-2);
    border: 1px solid var(--line);
    animation: rise 0.16s ease;
  }
  .row {
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .grow {
    flex: 1;
    min-width: 0;
  }
  .min {
    min-width: 0;
  }
  .small {
    font-size: 11.5px;
  }
  .note {
    margin: 0;
    font-size: 12.5px;
    color: var(--muted);
  }
  .meta {
    gap: 10px;
    flex-wrap: wrap;
  }
  .base {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .form label {
    display: flex;
    flex-direction: column;
    gap: 5px;
    font-size: 12px;
    color: var(--text-2);
  }
  .form label.check {
    flex-direction: row;
    align-items: center;
    gap: 7px;
  }
  .form label.check input {
    accent-color: var(--accent);
  }
  .acts {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
    margin: 4px 0 2px;
  }
  .two {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 8px;
  }
  .mrow {
    display: flex;
    align-items: center;
    gap: 5px;
    padding: 7px 8px;
    border-radius: 9px;
    background: var(--panel-solid);
    border: 1px solid var(--line);
  }
  .mt {
    display: flex;
    align-items: center;
    gap: 5px;
    font-size: 13px;
  }
  .msub {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .star {
    border: none;
    background: none;
    padding: 0 2px;
    font-size: 15px;
    line-height: 1;
    color: var(--line-2);
  }
  .star:hover {
    color: var(--warn);
  }
  .star.on {
    color: var(--warn);
  }
  .result {
    font-size: 12px;
    color: var(--text-2);
    padding: 8px 10px;
    border-radius: 9px;
    background: color-mix(in srgb, var(--ok) 8%, transparent);
    border: 1px solid color-mix(in srgb, var(--ok) 30%, transparent);
  }
  .result.bad {
    background: color-mix(in srgb, var(--err) 8%, transparent);
    border-color: color-mix(in srgb, var(--err) 32%, transparent);
  }
  .result .rt {
    display: flex;
    align-items: center;
    gap: 5px;
    color: var(--ok);
    font-weight: 600;
  }
  .result.bad .rt {
    color: var(--err);
  }
  .result p {
    margin: 5px 0 0;
    white-space: pre-wrap;
    word-break: break-word;
  }
  .errbox {
    font-size: 12px;
    color: var(--err);
    padding: 7px 9px;
    border-radius: 8px;
    background: color-mix(in srgb, var(--err) 9%, transparent);
    border: 1px solid color-mix(in srgb, var(--err) 32%, transparent);
    white-space: pre-wrap;
    word-break: break-word;
  }
  .remote {
    display: flex;
    flex-direction: column;
    gap: 3px;
    max-height: 160px;
    overflow: auto;
    padding: 4px;
    border-radius: 9px;
    border: 1px dashed var(--line-2);
  }
  .rrow {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 5px 7px;
    border-radius: 7px;
    border: none;
    background: none;
    color: var(--text);
    text-align: left;
    font-size: 12.5px;
  }
  .rrow:hover {
    background: color-mix(in srgb, var(--accent) 12%, transparent);
  }
</style>