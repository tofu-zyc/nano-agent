<script lang="ts">
  import { onMount } from 'svelte';
  import { api } from '../lib/api';
  import { store } from '../lib/state.svelte';
  import type { ModelEndpoint, ModelItem, ModelsView, RemoteModel } from '../lib/types';
  import Icon from './Icon.svelte';

  type Sel =
    | { kind: 'model'; id: string }
    | { kind: 'new'; endpoint: string }
    | { kind: 'endpoint'; id: string }
    | { kind: 'newendpoint' };

  let view = $state<ModelsView | null>(null);
  let sel = $state<Sel | null>(null);
  let q = $state('');
  let loading = $state(true);
  let busy = $state('');
  let err = $state('');
  let saved = $state(false);
  let testResult = $state<{ ok: boolean; reply?: string; error?: string; ms?: number; first_ms?: number } | null>(null);
  let remote = $state<RemoteModel[] | null>(null);
  let remoteErr = $state('');
  let afterEndpoint = $state(false);

  // 模型表单
  let mEndpoint = $state('');
  let mName = $state('');
  let mLabel = $state('');
  let mCtx = $state('');
  let mMax = $state('');
  let mHist = $state('');
  let mThink = $state('');
  let mLevels = $state('');
  let mImage = $state(false);

  // 接口表单
  let epName = $state('');
  let epKind = $state('openai');
  let epBase = $state('');
  let epKey = $state('');
  let epProxy = $state('');

  const eps = $derived(view?.endpoints ?? []);
  const models = $derived(view?.models ?? []);
  const selModel = $derived.by(() => {
    const s = sel;
    return s?.kind === 'model' ? models.find((m) => m.id === s.id) : undefined;
  });
  const selEndpoint = $derived.by(() => {
    const s = sel;
    return s?.kind === 'endpoint' ? eps.find((e) => e.id === s.id) : undefined;
  });
  const isNew = $derived(sel?.kind === 'new');
  const filtered = $derived.by(() => {
    const term = q.trim().toLowerCase();
    return eps
      .map((ep) => ({
        ep,
        list: modelsOn(ep.id).filter((m) => !term || `${m.label} ${m.name} ${ep.name} ${ep.base_url}`.toLowerCase().includes(term)),
      }))
      .filter((g) => !term || g.list.length > 0 || `${g.ep.name} ${g.ep.base_url}`.toLowerCase().includes(term));
  });
  $effect(() => {
    const intent = store.modelIntent;
    if (!intent) return;
    if (intent === 'endpoint') startEndpoint();
    else addModelFlow();
    store.modelIntent = null;
  });

  onMount(() => {
    load().then(() => {
      if (store.modelIntent) {
        const intent = store.modelIntent;
        store.modelIntent = null;
        if (intent === 'endpoint') startEndpoint();
        else addModelFlow();
      }
    });
  });

  const fail = (e: unknown) => (e instanceof Error ? e.message : String(e));
  const endpointOf = (id: string) => eps.find((e) => e.id === id);
  const modelsOn = (id: string) => models.filter((m) => m.endpoint === id);
  const intOf = (s: string) => {
    const n = parseInt(s, 10);
    return Number.isFinite(n) && n > 0 ? n : 0;
  };
  const levelsOf = () => mLevels.split(/[,，\s]+/).map((s) => s.trim()).filter(Boolean);

  async function load(selectId?: string) {
    loading = true;
    try {
      const v = await api.get<ModelsView>('/api/models');
      view = v;
      if (selectId) {
        const m = v.models.find((x) => x.id === selectId);
        if (m) {
          selectModel(m);
          return;
        }
      }
      const cur = sel;
      if (cur?.kind === 'model') {
        const m = v.models.find((x) => x.id === cur.id);
        if (m) {
          selectModel(m);
          return;
        }
      }
      if (cur) return;
      const m = v.models.find((x) => x.id === v.default) ?? v.models[0];
      if (m) selectModel(m);
      else sel = null;
    } catch (e) {
      err = fail(e);
    } finally {
      loading = false;
    }
  }

  function fillModel(m: ModelItem) {
    mEndpoint = m.endpoint;
    mName = m.name;
    mLabel = m.label === m.name ? '' : m.label;
    mCtx = m.context_window ? String(m.context_window) : '';
    mMax = m.max_tokens ? String(m.max_tokens) : '';
    mHist = m.max_history_tokens ? String(m.max_history_tokens) : '';
    mThink = m.thinking ?? '';
    mLevels = (m.thinking_levels ?? []).join(', ');
    mImage = (m.modalities ?? []).includes('image');
    remote = null;
    remoteErr = '';
    err = '';
    saved = false;
    testResult = null;
  }

  function selectModel(m: ModelItem) {
    sel = { kind: 'model', id: m.id };
    fillModel(m);
  }

  function startNew(endpoint: string) {
    const ep = endpointOf(endpoint) ?? eps[0];
    if (!ep) {
      afterEndpoint = true;
      startEndpoint();
      return;
    }
    sel = { kind: 'new', endpoint: ep.id };
    mEndpoint = ep.id;
    mName = '';
    mLabel = '';
    mCtx = '131072';
    mMax = '';
    mHist = '';
    mThink = '';
    mLevels = '';
    mImage = false;
    remote = null;
    remoteErr = '';
    err = '';
    saved = false;
    testResult = null;
  }

  function startEndpoint(id?: string) {
    afterEndpoint = !id; // 新建接口存好后自动接着加模型
    const e = id ? endpointOf(id) : undefined;
    epName = e?.name ?? '';
    epKind = e?.kind ?? 'openai';
    epBase = e?.base_url ?? '';
    epKey = '';
    epProxy = e?.proxy ?? '';
    err = '';
    sel = e ? { kind: 'endpoint', id: e.id } : { kind: 'newendpoint' };
  }

  function addModelFlow() {
    if (!eps.length) {
      afterEndpoint = true;
      startEndpoint();
      return;
    }
    startNew(eps[0].id);
  }

  async function saveModel() {
    if (!mName.trim()) {
      err = '模型名不能为空';
      return;
    }
    if (!intOf(mCtx)) {
      err = '上下文窗口要填一个正整数';
      return;
    }
    busy = 'save';
    err = '';
    try {
      const r = await api.post<{ id: string }>('/api/models/items', {
        endpoint: mEndpoint,
        name: mName.trim(),
        label: mLabel.trim(),
        context_window: intOf(mCtx),
        max_tokens: intOf(mMax),
        max_history_tokens: intOf(mHist),
        thinking: mThink.trim(),
        modalities: mImage ? ['text', 'image'] : ['text'],
        thinking_levels: levelsOf(),
      });
      await load(r.id);
      store.reloadModels();
      saved = true;
      setTimeout(() => (saved = false), 1600);
      store.toast({ kind: 'ok', text: '模型已保存' }, 1600);
    } catch (e) {
      err = fail(e);
    } finally {
      busy = '';
    }
  }

  async function saveEndpoint() {
    if (!epName.trim() && !epBase.trim()) {
      err = '名字和接口地址至少填一个';
      return;
    }
    busy = 'save';
    err = '';
    const body = { name: epName.trim(), kind: epKind, base_url: epBase.trim(), api_key: epKey, proxy: epProxy.trim() };
    try {
      const r =
        sel?.kind === 'endpoint'
          ? await api.put<{ id: string }>('/api/models/endpoints/' + encodeURIComponent(sel.id), body)
          : await api.post<{ id: string }>('/api/models/endpoints', body);
      const next = afterEndpoint;
      afterEndpoint = false;
      await load();
      if (next) startNew(r.id);
      else startEndpoint(r.id);
      store.reloadModels();
      store.toast({ kind: 'ok', text: '接口已保存' }, 1600);
    } catch (e) {
      err = fail(e);
    } finally {
      busy = '';
    }
  }

  async function removeEndpoint() {
    const s = sel;
    if (s?.kind !== 'endpoint') return;
    const n = modelsOn(s.id).length;
    if (!confirm(`删除接口「${epName}」？${n ? `它上面的 ${n} 个模型也会删掉，` : ''}机器上的服务不会受影响。`)) return;
    try {
      await api.del('/api/models/endpoints/' + encodeURIComponent(s.id));
      sel = null;
      await load();
      store.reloadModels();
      store.toast({ kind: 'ok', text: '接口已删除' }, 1600);
    } catch (e) {
      store.fail(e);
    }
  }

  async function removeModel() {
    const m = selModel;
    if (!m) return;
    if (!confirm(`删除模型「${m.label}」？单独选了它的会话会改回默认模型。`)) return;
    try {
      await api.del('/api/models/items?id=' + encodeURIComponent(m.id));
      sel = null;
      await load();
      store.reloadModels();
      store.toast({ kind: 'ok', text: '模型已删除' }, 1600);
    } catch (e) {
      store.fail(e);
    }
  }

  async function makeDefault() {
    const m = selModel;
    if (!m || m.default) return;
    try {
      await api.post('/api/models/default', { id: m.id });
      await load(m.id);
      store.reloadModels();
      store.toast({ kind: 'ok', text: `新会话默认用 ${m.label}` }, 2200);
    } catch (e) {
      store.fail(e);
    }
  }

  async function runTest() {
    const m = selModel;
    if (!m || busy === 'test') return;
    busy = 'test';
    testResult = null;
    try {
      const r = await api.post<{ ok: boolean; reply?: string; error?: string; ms?: number; first_ms?: number }>('/api/test/model', { id: m.id });
      testResult = { ok: !!r.ok, reply: r.reply, error: r.error, ms: r.ms, first_ms: r.first_ms };
    } catch (e) {
      testResult = { ok: false, error: fail(e) };
    } finally {
      busy = '';
    }
  }

  async function fetchRemote() {
    if (busy === 'fetch') return;
    busy = 'fetch';
    remote = null;
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

  const endpointLabel = (e?: ModelEndpoint) => (e ? e.name || e.base_url : '');
</script>
<div class="mp">
  <div class="bar">
    <div class="sum">
      <strong>模型和接口</strong>
      <span>{models.length} 个模型 · {eps.length} 个接口</span>
    </div>
    <button class="btn sm primary" onclick={addModelFlow} title="添加模型"><Icon name="plus" size={13} />模型</button>
    <button class="btn sm" onclick={() => { afterEndpoint = false; startEndpoint(); }} title="添加接口"><Icon name="plus" size={13} />接口</button>
  </div>

  {#if loading}
    <div class="center"><span class="spinner"></span>正在读取模型…</div>
  {:else if !eps.length && sel?.kind !== 'newendpoint'}
    <div class="center">
      <div class="bigicon"><Icon name="sparkles" size={24} /></div>
      <h3>还没有模型</h3>
      <p>先添加一个接口（模型服务），再给它添加模型。地址填到 /v1 为止，比如 https://api.deepseek.com/v1。</p>
      <button class="btn primary" onclick={() => { afterEndpoint = true; startEndpoint(); }}><Icon name="plus" size={14} />添加接口</button>
    </div>
  {:else}
    <div class="cols">
      <aside class="list">
        <div class="search">
          <Icon name="search" size={13} />
          <input bind:value={q} placeholder="搜索模型或接口" />
          {#if q}<button class="clr" onclick={() => (q = '')} aria-label="清空"><Icon name="x" size={11} /></button>{/if}
        </div>
        <div class="lscroll">
          {#each filtered as g (g.ep.id)}
            <div class="ep">
              <div class="eph">
                <button class="epname" onclick={() => startEndpoint(g.ep.id)} title="编辑接口">
                  <Icon name={g.ep.kind === 'mock' ? 'bot' : 'sparkles'} size={13} />
                  <span class="txt">{g.ep.name}</span>
                  <span class="tag">{g.ep.kind}</span>
                </button>
              </div>
              {#each g.list as m (m.id)}
                <button class="mrow" class:active={sel?.kind === 'model' && sel.id === m.id} onclick={() => selectModel(m)}>
                  <span class="mdot" class:def={m.default}></span>
                  <span class="mtext"><strong>{m.label}</strong><small>{m.name}</small></span>
                  <span class="mtags">
                    {#if m.default}<span class="tag accent">默认</span>{/if}
                    {#if m.modalities?.includes('image')}<span class="tag">图片</span>{/if}
                  </span>
                </button>
              {:else}
                <div class="emptyline">这个接口还没有模型</div>
              {/each}
            </div>
          {:else}
            <div class="emptyline">没有找到匹配的模型</div>
          {/each}
        </div>
      </aside>

      <section class="detail">
        {#if selModel || isNew}
          <div class="dwrap">
            <div class="dhead">
              <div class="grow">
                <h3>{selModel ? selModel.label : '添加模型'}</h3>
                <div class="sub">
                  {#if selModel}
                    <span>{endpointLabel(endpointOf(selModel.endpoint))}</span>
                    <span>{selModel.name}</span>
                    {#if selModel.default}<span class="tag accent">默认</span>{/if}
                    {#if selModel.modalities?.includes('image')}<span class="tag">图片</span>{/if}
                  {:else}
                    新模型会加到「{endpointLabel(endpointOf(mEndpoint))}」
                  {/if}
                </div>
              </div>
              {#if selModel}
                {#if !selModel.default}<button class="btn sm" onclick={makeDefault} title="新会话默认用它"><Icon name="check" size={12} />设为默认</button>{/if}
                <button class="btn sm" onclick={runTest} disabled={busy === 'test'}>
                  <Icon name={busy === 'test' ? 'refresh' : 'zap'} size={12} class={busy === 'test' ? 'spin' : ''} />
                  {busy === 'test' ? '测试中…' : '测试'}
                </button>
                <button class="btn sm ghost" onclick={removeModel} title="删除模型"><Icon name="trash" size={13} /></button>
              {/if}
            </div>

            {#if testResult}
              <div class="testbox" class:bad={!testResult.ok}>
                {#if testResult.ok}
                  <div class="tt"><Icon name="check" size={12} />通了 · 首字 {testResult.first_ms ?? '—'} ms · 总共 {testResult.ms ?? '—'} ms</div>
                  <p>{testResult.reply}</p>
                {:else}
                  <div class="tt"><Icon name="alert" size={12} />没通</div>
                  <p>{testResult.error}</p>
                {/if}
              </div>
            {/if}
            {#if err}<div class="errbox">{err}</div>{/if}

            <section class="card">
              <h4>接口</h4>
              {#if isNew}
                <label class="fl">放到哪个接口
                  <select class="input" bind:value={mEndpoint}>
                    {#each eps as e (e.id)}<option value={e.id}>{e.name}</option>{/each}
                  </select>
                </label>
                <p class="hint">也可以 <button class="lnk" onclick={() => { afterEndpoint = true; startEndpoint(); }}>新建一个接口</button>，存好后接着加模型。</p>
              {:else}
                <div class="epinfo">
                  <div>
                    <strong>{endpointLabel(endpointOf(mEndpoint))}</strong>
                    <code>{endpointOf(mEndpoint)?.base_url || '—'}</code>
                  </div>
                  <button class="btn sm" onclick={() => startEndpoint(mEndpoint)}><Icon name="sliders" size={12} />编辑接口</button>
                </div>
              {/if}
            </section>

            <section class="card">
              <h4>模型</h4>
              {#if isNew}
                <div class="fetchrow">
                  <button class="btn sm" onclick={fetchRemote} disabled={busy === 'fetch'}>
                    <Icon name={busy === 'fetch' ? 'refresh' : 'down'} size={12} class={busy === 'fetch' ? 'spin' : ''} />
                    {busy === 'fetch' ? '正在读取…' : '从接口读取模型列表'}
                  </button>
                  <span class="muted small">或者直接填</span>
                </div>
                {#if remoteErr}<div class="errbox">{remoteErr}</div>{/if}
                {#if remote}
                  <div class="remote">
                    {#each remote as r (r.id)}
                      <button class="rrow" onclick={() => pickRemote(r)}>
                        <span class="grow"><code>{r.id}</code>{#if r.added}<span class="tag">已添加</span>{/if}</span>
                        {#if r.modalities?.includes('image')}<span class="tag">图片</span>{/if}
                        {#if r.context_window}<span class="muted small">{r.context_window}</span>{/if}
                      </button>
                    {:else}
                      <p class="hint">接口没有返回模型。</p>
                    {/each}
                  </div>
                {/if}
              {/if}
              <div class="grid2">
                <label class="fl">模型名
                  <input class="input" bind:value={mName} placeholder="接口里的模型名，比如 deepseek-chat" />
                </label>
                <label class="fl">显示名
                  <input class="input" bind:value={mLabel} placeholder="留空就用模型名" />
                </label>
              </div>
              <div class="grid3">
                <label class="fl">上下文窗口
                  <input class="input" bind:value={mCtx} inputmode="numeric" placeholder="必填，比如 131072" />
                </label>
                <label class="fl">最大输出
                  <input class="input" bind:value={mMax} inputmode="numeric" placeholder="留空不限制" />
                </label>
                <label class="fl">历史上限
                  <input class="input" bind:value={mHist} inputmode="numeric" placeholder="留空按窗口一半" />
                </label>
              </div>
              <div class="grid2">
                <label class="fl">默认思考强度
                  <input class="input" bind:value={mThink} placeholder="比如 high，留空不传" />
                </label>
                <label class="fl">可选的思考强度
                  <input class="input" bind:value={mLevels} placeholder="none, low, high" />
                </label>
              </div>
              <div class="chips">
                <span class="chiplabel">输入类型</span>
                <span class="chip on locked" title="文字总是支持">文字</span>
                <button class="chip" class:on={mImage} onclick={() => (mImage = !mImage)} title="多模态模型才勾">图片</button>
              </div>
            </section>

            <div class="savebar">
              {#if saved}<span class="ok"><Icon name="check" size={11} />已保存</span>{/if}
              <button class="btn primary" onclick={saveModel} disabled={busy === 'save' || !mName.trim()}>
                {busy === 'save' ? '正在保存…' : '保存'}
              </button>
            </div>
          </div>
        {:else if selEndpoint || sel?.kind === 'newendpoint'}
          <div class="dwrap">
            <div class="dhead">
              <div class="grow">
                <h3>{selEndpoint ? '编辑接口' : '添加接口'}</h3>
                <div class="sub">{selEndpoint ? selEndpoint.base_url || '' : '一个接口上可以放多个模型'}</div>
              </div>
            </div>
            {#if err}<div class="errbox">{err}</div>{/if}
            <section class="card">
              {#if afterEndpoint}<p class="hint">保存后接着给这个接口添加模型。</p>{/if}
              <label class="fl">名字<input class="input" bind:value={epName} placeholder="比如 本地 vLLM" /></label>
              <div class="grid2">
                <label class="fl">类型
                  <select class="input" bind:value={epKind}>
                    <option value="openai">openai（兼容接口）</option>
                    <option value="mock">mock（假模型，调试用）</option>
                  </select>
                </label>
                <label class="fl">代理<input class="input" bind:value={epProxy} placeholder="留空用默认代理；none 直连" /></label>
              </div>
              {#if epKind === 'openai'}
                <label class="fl">接口地址<input class="input" bind:value={epBase} placeholder="https://api.deepseek.com/v1" /></label>
              {/if}
              <label class="fl">API Key
                <input class="input" type="password" bind:value={epKey} autocomplete="new-password"
                  placeholder={selEndpoint?.has_key ? '已经设置，留空表示不改' : '内网接口可以留空'} />
              </label>
            </section>
            <div class="savebar">
              {#if selEndpoint}<button class="btn ghost danger" onclick={removeEndpoint}><Icon name="trash" size={12} />删除接口</button>{/if}
              <span class="grow"></span>
              <button class="btn" onclick={() => { afterEndpoint = false; sel = null; load(); }}>取消</button>
              <button class="btn primary" onclick={saveEndpoint} disabled={busy === 'save'}>{busy === 'save' ? '正在保存…' : '保存'}</button>
            </div>
          </div>
        {:else}
          <div class="center">
            <div class="bigicon"><Icon name="sparkles" size={24} /></div>
            <h3>添加一个模型</h3>
            <p>左边选一个模型编辑，或者先给接口添加模型。</p>
            <button class="btn primary" onclick={addModelFlow}><Icon name="plus" size={13} />添加模型</button>
          </div>
        {/if}
      </section>
    </div>
  {/if}
</div>
<style>
  .mp {
    height: 100%;
    display: flex;
    flex-direction: column;
    min-height: 0;
  }
  .bar {
    display: flex;
    align-items: center;
    gap: 7px;
    padding: 2px 2px 10px;
    border-bottom: 1px solid var(--line);
    margin-bottom: 10px;
  }
  .sum {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 1px;
  }
  .sum strong {
    font-size: 13px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .sum span {
    font-size: 11px;
    color: var(--muted);
  }
  .cols {
    flex: 1;
    min-height: 0;
    display: flex;
    gap: 12px;
  }
  .list {
    width: 250px;
    flex: none;
    display: flex;
    flex-direction: column;
    min-height: 0;
    border-right: 1px solid var(--line);
    padding-right: 8px;
  }
  .search {
    display: flex;
    align-items: center;
    gap: 6px;
    height: 30px;
    padding: 0 9px;
    border-radius: 8px;
    border: 1px solid var(--line);
    background: var(--bg-2);
    color: var(--muted);
    margin-bottom: 8px;
  }
  .search input {
    flex: 1;
    min-width: 0;
    border: none;
    background: none;
    outline: none;
    font-size: 12px;
    color: var(--text);
  }
  .clr {
    border: none;
    background: none;
    color: var(--muted);
    padding: 0;
  }
  .lscroll {
    flex: 1;
    overflow: auto;
    padding-right: 2px;
  }
  .ep {
    margin-bottom: 4px;
  }
  .eph {
    display: flex;
    align-items: center;
  }
  .epname {
    flex: 1;
    min-width: 0;
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 5px 6px;
    border: none;
    background: none;
    border-radius: 7px;
    color: var(--text-2);
    font-size: 11.5px;
    text-align: left;
  }
  .epname:hover {
    background: var(--panel-2);
    color: var(--text);
  }
  .epname .txt {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .mrow {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 6px 7px;
    margin: 1px 0;
    border: 1px solid transparent;
    border-radius: 8px;
    background: none;
    text-align: left;
    color: var(--text-2);
  }
  .mrow:hover {
    background: var(--panel-2);
    color: var(--text);
  }
  .mrow.active {
    background: color-mix(in srgb, var(--c-model) 12%, var(--panel-2));
    border-color: color-mix(in srgb, var(--c-model) 35%, var(--line));
    color: var(--text);
  }
  .mdot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--line-2);
    flex: none;
  }
  .mdot.def {
    background: var(--ok);
  }
  .mtext {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 0;
  }
  .mtext strong {
    font-size: 12px;
    font-weight: 550;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .mtext small {
    font-size: 10.5px;
    color: var(--muted);
    font-family: var(--mono);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .mtags {
    display: flex;
    gap: 2px;
    flex: none;
  }
  .emptyline {
    padding: 5px 8px;
    font-size: 11px;
    color: var(--muted);
  }
  .detail {
    flex: 1;
    min-width: 0;
    overflow: auto;
    padding-right: 2px;
  }
  .dwrap {
    max-width: 520px;
  }
  .dhead {
    display: flex;
    align-items: flex-start;
    gap: 7px;
    margin-bottom: 10px;
  }
  .dhead h3 {
    margin: 0;
    font-size: 15px;
  }
  .sub {
    display: flex;
    align-items: center;
    gap: 5px;
    flex-wrap: wrap;
    margin-top: 3px;
    font-size: 11px;
    color: var(--muted);
  }
  .card {
    border: 1px solid var(--line);
    border-radius: 11px;
    background: var(--panel-2);
    padding: 11px 12px 12px;
    margin-bottom: 10px;
  }
  .card h4 {
    margin: 0 0 8px;
    font-size: 11px;
    font-weight: 600;
    color: var(--muted);
    letter-spacing: 0.4px;
  }
  .fl {
    display: flex;
    flex-direction: column;
    gap: 4px;
    margin-bottom: 8px;
    font-size: 11.5px;
    color: var(--text-2);
  }
  .grid2 {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 0 10px;
  }
  .grid3 {
    display: grid;
    grid-template-columns: 1fr 1fr 1fr;
    gap: 0 10px;
  }
  .hint {
    margin: 0 0 8px;
    font-size: 11.5px;
    line-height: 1.55;
    color: var(--muted);
  }
  .lnk {
    border: none;
    background: none;
    color: var(--accent-2);
    padding: 0;
    text-decoration: underline;
    font-size: inherit;
  }
  .epinfo {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .epinfo > div {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .epinfo code {
    font-size: 10.5px;
    color: var(--muted);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .chips {
    display: flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
    margin-top: 2px;
  }
  .chiplabel {
    font-size: 11.5px;
    color: var(--text-2);
  }
  .chip {
    height: 25px;
    padding: 0 10px;
    border-radius: 7px;
    border: 1px solid var(--line-2);
    background: var(--bg-2);
    color: var(--text-2);
    font-size: 11.5px;
  }
  .chip.on {
    border-color: color-mix(in srgb, var(--accent) 55%, var(--line));
    background: color-mix(in srgb, var(--accent) 14%, var(--panel-solid));
    color: var(--text);
  }
  .chip.locked {
    cursor: default;
  }
  .fetchrow {
    display: flex;
    align-items: center;
    gap: 7px;
    margin-bottom: 8px;
  }
  .remote {
    border: 1px solid var(--line);
    border-radius: 8px;
    margin-bottom: 8px;
    max-height: 180px;
    overflow: auto;
  }
  .rrow {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 6px 8px;
    border: none;
    border-bottom: 1px solid var(--line);
    background: none;
    text-align: left;
    color: var(--text-2);
    font-size: 11.5px;
  }
  .rrow:last-child {
    border-bottom: none;
  }
  .rrow:hover {
    background: var(--panel-2);
    color: var(--text);
  }
  .savebar {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 2px 0 4px;
  }
  .savebar .ok {
    display: inline-flex;
    align-items: center;
    gap: 3px;
    color: var(--ok);
    font-size: 11px;
  }
  .savebar .btn.primary {
    margin-left: auto;
  }
  .savebar .grow {
    flex: 1;
  }
  .testbox {
    margin: 0 0 9px;
    padding: 8px 10px;
    border-radius: 9px;
    border: 1px solid color-mix(in srgb, var(--ok) 40%, var(--line));
    background: color-mix(in srgb, var(--ok) 7%, var(--panel-2));
    font-size: 11.5px;
  }
  .testbox.bad {
    border-color: color-mix(in srgb, var(--err) 45%, var(--line));
    background: color-mix(in srgb, var(--err) 7%, var(--panel-2));
  }
  .tt {
    display: flex;
    align-items: center;
    gap: 5px;
    font-weight: 600;
  }
  .testbox p {
    margin: 4px 0 0;
    color: var(--text-2);
    white-space: pre-wrap;
  }
  .errbox {
    margin: 0 0 9px;
    padding: 7px 10px;
    border-radius: 8px;
    border: 1px solid color-mix(in srgb, var(--err) 45%, var(--line));
    background: color-mix(in srgb, var(--err) 8%, var(--panel-2));
    color: var(--text);
    font-size: 11.5px;
  }
  .center {
    height: 100%;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    text-align: center;
    color: var(--text-2);
    padding: 24px;
  }
  .center h3 {
    margin: 10px 0 4px;
    font-size: 14px;
    color: var(--text);
  }
  .center p {
    max-width: 340px;
    margin: 0 0 12px;
    font-size: 11.5px;
    line-height: 1.6;
  }
  .bigicon {
    width: 46px;
    height: 46px;
    border-radius: 14px;
    display: grid;
    place-items: center;
    color: var(--c-model);
    background: color-mix(in srgb, var(--c-model) 12%, transparent);
  }
  .spinner {
    width: 11px;
    height: 11px;
    border-radius: 50%;
    border: 2px solid var(--accent);
    border-top-color: transparent;
    animation: spin 0.8s linear infinite;
    display: inline-block;
    margin-right: 6px;
  }
  .danger {
    color: var(--err);
  }
</style>