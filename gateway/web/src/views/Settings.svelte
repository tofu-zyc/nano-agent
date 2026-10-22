<script lang="ts">
  import { store } from '../lib/state.svelte';
  import { score } from '../lib/fuzzy';
  import type { Target } from '../lib/types';
  import Field from '../components/Field.svelte';
  import Icon from '../components/Icon.svelte';

  const groups: { id: string; label: string; icon: string; color: string; desc: string; target: Target }[] = [
    { id: 'model', label: '模型', icon: 'sparkles', color: 'var(--c-model)', desc: '接口、思考强度、上下文长度', target: { type: 'model' } },
    { id: 'workspace', label: '工作目录与沙箱', icon: 'folder', color: 'var(--c-dir)', desc: '工具能碰到的范围、默认权限模式', target: { type: 'workspace' } },
    { id: 'gateway', label: '网关', icon: 'hub', color: 'var(--c-gateway)', desc: '管理员、访客权限、消息时间、网页登录', target: { type: 'gateway' } },
    { id: 'telegram', label: 'Telegram', icon: 'send', color: 'var(--c-telegram)', desc: '机器人 token、代理', target: { type: 'channel', name: 'telegram' } },
    { id: 'feishu', label: '飞书', icon: 'feishu', color: 'var(--c-feishu)', desc: '扫码创建机器人、应用凭证、代理', target: { type: 'channel', name: 'feishu' } },
    { id: 'cli', label: '命令行', icon: 'terminal', color: 'var(--c-cli)', desc: '终端里显示什么', target: { type: 'channel', name: 'cli' } },
    { id: 'kernel', label: '内核连接', icon: 'shield', color: 'var(--c-kernel)', desc: '监听地址和口令', target: { type: 'kernel' } },
  ];

  let q = $state('');
  let onlyChanged = $state(false);
  let showAdvanced = $state(false);
  let selected = $state('model');

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

    {#if pending}
      <div class="banner"><Icon name="refresh" size={14} />有 {pending} 项改动要重启网关才生效（重启时用同一个配置文件即可，网页里的修改会保留）。</div>
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
            <button class="btn sm" onclick={() => store.openOnMap(g.target)}>
              <Icon name="graph" size={13} />在拓扑图里看
            </button>
          </div>
          <div class="fields">
            {#if g.id === 'model'}
              <div class="callout">
                <Icon name="sparkles" size={16} />
                <div class="grow">
                  <strong>可以加多个接口和模型</strong>
                  <p>配置文件里的 [llm] 是内置的接口和模型。别的 OpenAI 兼容接口、多模态模型在模型面板里添加，每个会话可以用 <code>/model</code> 单独选。</p>
                </div>
                <button class="btn sm primary" onclick={() => store.addModel()}><Icon name="plus" size={13} />添加模型</button>
              </div>
            {/if}
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
  .callout {
    display: flex;
    align-items: flex-start;
    gap: 10px;
    margin: 4px 0 10px;
    padding: 12px 14px;
    border-radius: 12px;
    background: color-mix(in srgb, var(--c) 8%, var(--panel-2));
    border: 1px solid color-mix(in srgb, var(--c) 30%, var(--line));
  }
  .callout > :global(svg) {
    color: var(--c);
    margin-top: 1px;
  }
  .callout strong {
    font-size: 13px;
  }
  .callout p {
    margin: 3px 0 0;
    font-size: 12px;
    line-height: 1.6;
    color: var(--text-2);
  }
  .callout .grow {
    flex: 1;
    min-width: 0;
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
</style>
