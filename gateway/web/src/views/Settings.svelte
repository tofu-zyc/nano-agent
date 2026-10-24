<script lang="ts">
  import { store } from '../lib/state.svelte';
  import { score } from '../lib/fuzzy';
  import type { Target } from '../lib/types';
  import Field from '../components/Field.svelte';
  import Icon from '../components/Icon.svelte';

  const groups: { id: string; label: string; icon: string; color: string; desc: string; target?: Target }[] = [
    { id: 'memory', label: '上下文与记忆', icon: 'brain', color: 'var(--c-memory)', desc: '上下文什么时候整理、整理到多少、空闲整理、归档保留多久', target: { type: 'memory' } },
    { id: 'chat', label: '消息与显示', icon: 'chat', color: 'var(--c-session)', desc: '聊天里显示多少过程（思考、工具）、消息带不带时间、时区' },
    { id: 'search', label: '网络搜索', icon: 'search', color: 'var(--c-search)', desc: '让模型能搜网页、读网页（DeepSeek 官方接口，按次计费）', target: { type: 'search' } },
    { id: 'llm', label: '模型请求', icon: 'sparkles', color: 'var(--c-model)', desc: '请求超时、思考回传、默认温度、额外参数（模型和接口在拓扑图的「模型」节点）', target: { type: 'model' } },
    { id: 'workspace', label: '工作目录与沙箱', icon: 'folder', color: 'var(--c-dir)', desc: '工具能碰到的范围、默认权限模式', target: { type: 'workspace' } },
    { id: 'gateway', label: '网关', icon: 'hub', color: 'var(--c-gateway)', desc: '管理员、访客权限、代理、更新、网页登录', target: { type: 'gateway' } },
    { id: 'telegram', label: 'Telegram', icon: 'send', color: 'var(--c-telegram)', desc: '机器人 token、代理', target: { type: 'channel', name: 'telegram' } },
    { id: 'feishu', label: '飞书', icon: 'feishu', color: 'var(--c-feishu)', desc: '扫码创建机器人、应用凭证、代理', target: { type: 'channel', name: 'feishu' } },
    { id: 'cli', label: '命令行', icon: 'terminal', color: 'var(--c-cli)', desc: '终端里显示什么', target: { type: 'channel', name: 'cli' } },
    { id: 'kernel', label: '内核', icon: 'shield', color: 'var(--c-kernel)', desc: '每条消息最多请求模型几次、最长多久、等审批多久；内核怎么连进来', target: { type: 'kernel' } },
  ];

  let q = $state('');
  let onlyChanged = $state(false);
  let showAdvanced = $state(false);
  let selected = $state('memory');

  const focus = $derived(store.route.focus ?? null);
  $effect(() => {
    if (!focus) return;
    const f = store.field(focus);
    if (f) {
      selected = f.group;
      if (f.advanced) showAdvanced = true;
      queueMicrotask(() => document.getElementById('f-' + focus)?.scrollIntoView({ block: 'center', behavior: 'smooth' }));
    }
  });

  const filtered = $derived(
    store.fields.filter(
      (f) =>
        (!onlyChanged || f.source === 'web') &&
        (showAdvanced || !f.advanced || q || onlyChanged) &&
        (!q || score(q, `${f.label} ${f.key} ${f.help ?? ''} ${f.keywords ?? ''} ${f.section ?? ''}`) > 0),
    ),
  );
  const searching = $derived(!!q.trim() || onlyChanged);
  const visible = $derived(searching ? groups.filter((g) => filtered.some((f) => f.group === g.id)) : groups.filter((g) => g.id === selected));
  const changed = $derived(store.fields.filter((f) => f.source === 'web').length);
  const pending = $derived(store.fields.filter((f) => f.pending).length);
</script>

<div class="settings">
  <nav class="groups">
    {#each groups as g}
      {@const n = store.fields.filter((f) => f.group === g.id).length}
      {@const c = store.fields.filter((f) => f.group === g.id && f.source === 'web').length}
      <button class="g" class:active={!searching && selected === g.id} style="--c: {g.color}" onclick={() => { selected = g.id; q = ''; onlyChanged = false; }}>
        <span class="ic"><Icon name={g.icon} size={15} /></span>
        <span class="gl">{g.label}</span>
        {#if c}<span class="dotc" title="{c} 项在网页里改过"></span>{/if}
        <span class="n">{n}</span>
      </button>
    {/each}
    <div class="legend">
      <div><i class="web"></i>网页里改过</div>
      <div><i class="file"></i>来自配置文件</div>
      <div><i class="env"></i>来自环境变量</div>
      <div><i></i>默认值</div>
      <p class="autosave">改完按回车或点到别处就自动保存，旁边会出现“已保存”</p>
    </div>
  </nav>

  <section class="content">
    <div class="bar">
      <div class="search">
        <Icon name="search" size={15} />
        <input bind:value={q} placeholder="搜索配置项，比如“代理”、“超时”、“token”" />
        {#if q}<button class="clear" onclick={() => (q = '')}><Icon name="x" size={13} /></button>{/if}
      </div>
      <label class="toggle"><input type="checkbox" bind:checked={onlyChanged} />只看改过的（{changed}）</label>
      <label class="toggle"><input type="checkbox" bind:checked={showAdvanced} />显示高级设置</label>
    </div>

    {#if store.ov?.runtime}
      {@const rt = store.ov.runtime}
      {@const up = rt.update}
      <div class="version-row">
        <Icon name="hub" size={14} />
        <span>当前版本 <b>{rt.version}</b></span>
        {#if up?.available}
          <span class="new">新版本 {up.latest}</span>
          {#if up.updatable}
            <button class="btn sm primary" disabled={store.updating || store.restarting} onclick={() => store.updateGateway()}>
              <Icon name="refresh" size={13} />{store.updating ? '更新中…' : `更新到 ${up.latest}`}
            </button>
          {:else}
            <span class="muted" title={up.why}>这个网关不能自动更新（不是安装脚本装的）</span>
          {/if}
        {:else if up?.checked && !up.error}
          <span class="muted">已经是最新版本</span>
        {/if}
        {#if up?.error}<span class="err">检查失败：{up.error}</span>{/if}
        <button class="btn sm" disabled={store.checkingUpdate || store.updating} onclick={() => store.checkUpdate()}>
          {store.checkingUpdate ? '检查中…' : '检查更新'}
        </button>
        <span class="muted hint">更新会备份旧版本，新版本起不来自动退回；接入的电脑上的内核会自动换成同一个版本</span>
      </div>
    {/if}
    {#if store.updating && !store.restarting}
      <div class="banner restarting"><Icon name="refresh" size={14} />正在下载并校验新版本…</div>
    {/if}
    {#if pending}
      <div class="banner"><Icon name="refresh" size={14} />有 {pending} 项改动要重启网关才生效（重启时用同一个配置文件即可，网页里的修改会保留）。
        <button class="btn sm" disabled={store.restarting} onclick={() => store.restartGateway()}>
          <Icon name="refresh" size={13} />{store.restarting ? '重启中…' : '重启网关'}
        </button>
      </div>
    {/if}
    {#if store.restarting}
      <div class="banner restarting"><Icon name="refresh" size={14} />网关正在重启，页面会自动重连…</div>
    {/if}

    <div class="scroll">
      {#each visible as g (g.id)}
        {@const fs = filtered.filter((f) => f.group === g.id)}
        <div class="block" style="--c: {g.color}">
          <div class="bh">
            <span class="ic"><Icon name={g.icon} size={17} /></span>
            <div>
              <h3>{g.label}</h3>
              <p>{g.desc}</p>
            </div>
            {#if g.target}
              <button class="btn sm" onclick={() => store.openOnMap(g.target!)}>
                <Icon name="graph" size={13} />在拓扑图里看
              </button>
            {/if}
          </div>
          <div class="fields">
            {#each fs as f (f.key)}
              <Field {f} highlight={focus === f.key} />
            {:else}
              <p class="muted none">这一组没有{onlyChanged ? '改过的' : ''}配置项。</p>
            {/each}
          </div>
        </div>
      {:else}
        <div class="empty muted">没有找到“{q}”相关的配置项</div>
      {/each}
    </div>
  </section>
</div>

<style>
  .settings {
    flex: 1;
    min-width: 0;
    display: flex;
  }
  .groups {
    width: 230px;
    flex: none;
    padding: 14px 10px;
    border-right: 1px solid var(--line);
    display: flex;
    flex-direction: column;
    gap: 3px;
  }
  .g {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 8px 10px;
    border: none;
    border-radius: 11px;
    background: none;
    text-align: left;
    color: var(--text-2);
  }
  .g:hover {
    background: var(--panel-2);
    color: var(--text);
  }
  .g.active {
    background: color-mix(in srgb, var(--c) 14%, transparent);
    color: var(--text);
  }
  .g .ic {
    width: 28px;
    height: 28px;
    border-radius: 9px;
    display: grid;
    place-items: center;
    color: var(--c);
    background: color-mix(in srgb, var(--c) 14%, transparent);
  }
  .gl {
    flex: 1;
    font-size: 13px;
  }
  .n {
    font-size: 11px;
    color: var(--muted);
  }
  .dotc {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--accent);
  }
  .autosave {
    margin: 6px 0 0;
    line-height: 1.5;
  }
  .legend {
    margin-top: auto;
    padding: 10px;
    font-size: 11.5px;
    color: var(--muted);
    display: flex;
    flex-direction: column;
    gap: 5px;
  }
  .legend i {
    display: inline-block;
    width: 7px;
    height: 7px;
    border-radius: 50%;
    margin-right: 7px;
    background: var(--line-2);
  }
  .legend i.web {
    background: var(--accent);
  }
  .legend i.file {
    background: var(--text-2);
  }
  .legend i.env {
    background: var(--c-model);
  }
  .content {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  .bar {
    display: flex;
    align-items: center;
    gap: 16px;
    padding: 14px 24px 10px;
  }
  .search {
    flex: 1;
    max-width: 440px;
    display: flex;
    align-items: center;
    gap: 9px;
    height: 36px;
    padding: 0 12px;
    border-radius: 11px;
    background: var(--bg-2);
    border: 1px solid var(--line);
    color: var(--muted);
  }
  .search:focus-within {
    border-color: var(--accent);
  }
  .search input {
    flex: 1;
    border: none;
    outline: none;
    background: none;
    font-size: 13.5px;
    color: var(--text);
  }
  .clear {
    border: none;
    background: none;
    color: var(--muted);
    display: grid;
  }
  .toggle {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 12.5px;
    color: var(--text-2);
    cursor: pointer;
    user-select: none;
  }
  .toggle input {
    accent-color: var(--accent);
  }
  .banner {
    margin: 0 24px 6px;
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 9px 12px;
    border-radius: 11px;
    font-size: 12.5px;
    color: var(--warn);
    background: color-mix(in srgb, var(--warn) 8%, transparent);
    border: 1px solid color-mix(in srgb, var(--warn) 30%, transparent);
  }
  .scroll {
    flex: 1;
    overflow: auto;
    padding: 6px 24px 40px;
  }
  .block {
    max-width: 820px;
    margin-bottom: 22px;
    animation: rise 0.25s ease;
  }
  .bh {
    display: flex;
    align-items: center;
    gap: 12px;
    margin-bottom: 10px;
  }
  .bh > div {
    flex: 1;
  }
  .bh .ic {
    width: 38px;
    height: 38px;
    border-radius: 12px;
    display: grid;
    place-items: center;
    color: var(--c);
    background: color-mix(in srgb, var(--c) 15%, transparent);
  }
  h3 {
    margin: 0;
    font-size: 15px;
  }
  .bh p {
    margin: 0;
    font-size: 12.5px;
    color: var(--muted);
  }

  .fields {
    border-radius: 14px;
    background: var(--panel);
    border: 1px solid var(--line);
  }
  .fields :global(.field + .field) {
    border-top: 1px solid var(--line);
    border-radius: 0;
  }
  .none {
    padding: 16px;
    margin: 0;
  }
  .empty {
    padding: 60px;
    text-align: center;
  }
  .version-row {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 8px 12px;
    padding: 10px 14px;
    margin-bottom: 10px;
    border: 1px solid var(--line);
    border-radius: 10px;
    background: var(--panel);
    font-size: 13px;
  }
  .version-row .new {
    color: var(--accent);
    font-weight: 600;
  }
  .version-row .err {
    color: var(--err);
  }
  .version-row .hint {
    flex-basis: 100%;
    font-size: 12px;
  }
</style>
