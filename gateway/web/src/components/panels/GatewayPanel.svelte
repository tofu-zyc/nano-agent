<script lang="ts">
  import { store } from '../../lib/state.svelte';
  import { homeShort, num, pct, since, truncate } from '../../lib/format';
  import { homeOf, strayFields } from '../../lib/nodes';
  import type { FieldState } from '../../lib/types';
  import FieldGroup from '../FieldGroup.svelte';
  import Field from '../Field.svelte';
  import Icon from '../Icon.svelte';

  const ov = $derived(store.ov!);
  const t = $derived(ov.totals);
  const rt = $derived(ov.runtime);
  const up = $derived(rt?.update);
  const pending = $derived(store.fields.filter((f) => f.pending));
  // 网页里改过的设置：在哪个节点、现在是什么，点一下去那个节点
  const changed = $derived(store.fields.filter((f) => f.source === 'web' && f.type !== 'secret'));
  const stray = $derived(strayFields(store.fields));

  function preview(f: FieldState) {
    if (f.type === 'bool') return f.value ? '开' : '关';
    if (Array.isArray(f.value)) return f.value.length ? f.value.join('、') : '空';
    if (f.value === null || f.value === undefined || f.value === '') return '空';
    return truncate(String(typeof f.value === 'object' ? JSON.stringify(f.value) : f.value), 28);
  }
</script>

<div class="pnl">
  <div class="p-card" class:warn={up?.available}>
    <div class="p-row">
      <Icon name="hub" size={14} />
      <span class="p-grow">当前版本 <b>{rt?.version ?? '—'}</b>{#if rt} · 已运行 {since(rt.started)}{/if}</span>
      {#if up?.available}<span class="tag accent">新版本 {up.latest}</span>{:else if up?.checked && !up.error}<span class="tag ok">已经是最新</span>{/if}
    </div>
    {#if up?.error}<div class="p-err">检查失败：{up.error}</div>{/if}
    {#if up && !up.updatable && up.why}<p class="p-note">{up.installed ? '安装版 · 手动升级' : '当前程序布局'}：{up.why}</p>{/if}
    <div class="p-actions tight">
      {#if up?.available && up.updatable}
        <button class="btn sm primary" disabled={store.updating || store.restarting} onclick={() => store.updateGateway()}>
          <Icon name="refresh" size={13} />{store.updating ? '更新中…' : `更新到 ${up.latest}`}
        </button>
      {/if}
      <button class="btn sm" disabled={store.checkingUpdate || store.updating} onclick={() => store.checkUpdate()}>
        {store.checkingUpdate ? '检查中…' : '检查更新'}
      </button>
      <button class="btn sm" disabled={store.restarting} onclick={() => confirm('重启网关？正在处理的消息会中断，页面会自动重连。') && store.restartGateway()}>
        <Icon name="refresh" size={13} />{store.restarting ? '重启中…' : '重启网关'}
      </button>
    </div>
    <p class="p-note p-small">更新会备份旧版本，新版本起不来自动退回；接入的电脑上的内核会自动换成同一个版本。检查更新和下载走下面的「默认代理」。</p>
  </div>

  {#if pending.length}
    <div class="p-warn">
      有 {pending.length} 项改动要重启网关才生效：{pending.map((f) => f.label).join('、')}。上面点「重启网关」，网页里的修改会保留。
    </div>
  {/if}

  <div class="p-grid" style="margin-top: 14px">
    <div class="p-stat"><b>{t.runs}</b><span>处理轮数</span></div>
    <div class="p-stat"><b>{t.model_calls}</b><span>模型调用</span></div>
    <div class="p-stat"><b class="acc">{pct(t.cached_tokens, t.prompt_tokens)}</b><span>前缀缓存命中</span></div>
    <div class="p-stat"><b>{num(t.prompt_tokens)}</b><span>输入 tokens</span></div>
    <div class="p-stat"><b>{num(t.completion_tokens)}</b><span>输出 tokens</span></div>
    <div class="p-stat"><b>{num(t.reasoning_tokens)}</b><span>其中思考</span></div>
  </div>
  <div class="p-kv">
    <div><span>配置文件</span><code>{homeShort(ov.gateway.config_dir)}/gateway.toml</code></div>
    <div><span>数据目录</span><code>{homeShort(ov.gateway.data_dir)}</code></div>
    <div><span>内核监听</span><code>{ov.gateway.kernel_listen}</code></div>
  </div>

  <FieldGroup group="gateway" focus={store.focus} />

  <h5 class="p-h">网页里改过的设置 <span class="muted p-small">{changed.length ? `${changed.length} 项，点一下去它所在的节点` : '都还是配置文件或默认值'}</span></h5>
  {#if changed.length}
    <div class="p-card">
      {#each changed as f (f.key)}
        {@const h = homeOf(f)}
        <button class="p-srow" onclick={() => store.open(h.target, f.key)}>
          <span class="dot" style="background: {h.color}"></span>
          <span class="muted p-nowrap">{h.label}</span>
          <span class="p-grow">{f.label}</span>
          <code class="muted">{preview(f)}</code>
          <Icon name="right" size={13} />
        </button>
      {/each}
    </div>
  {/if}

  {#if stray.length}
    <h5 class="p-h">其他设置 <span class="muted p-small">网页还不认识它们该挂在哪个节点</span></h5>
    <div class="stray">
      {#each stray as f (f.key)}
        <Field {f} highlight={store.focus === f.key} />
      {/each}
    </div>
  {/if}
</div>

<style>
  .stray {
    border-radius: 14px;
    background: var(--panel-2);
    border: 1px solid var(--line);
  }
  .stray :global(.field + .field) {
    border-top: 1px solid var(--line);
  }
</style>
