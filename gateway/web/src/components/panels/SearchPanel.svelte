<script lang="ts">
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import FieldGroup from '../FieldGroup.svelte';
  import Icon from '../Icon.svelte';

  const si = $derived(store.ov?.search);
  const ready = $derived(!!si?.enabled && !!si?.key);
  const keyText = $derived.by(() => {
    const k = si?.key ?? '';
    if (k === 'settings') return '用下面填的 key';
    if (k.startsWith('endpoint:')) return `借用模型接口「${k.slice(9)}」的 key`;
    return '没有：在下面填 DeepSeek 的 API key，或者在模型节点添加一个地址是 api.deepseek.com 的接口';
  });
  let testing = $state(false);
  async function test() {
    testing = true;
    try {
      const r = await api.post<{ ok: boolean; error?: string; results?: number; ms?: number }>('/api/test/search', {});
      if (!r.ok) throw new Error(r.error || '没通');
      store.toast({ kind: 'ok', text: `搜索正常 · ${r.results} 条结果 · ${r.ms} ms` }, 2600);
    } catch (err) {
      store.fail(err);
    } finally {
      testing = false;
    }
  }
</script>

<div class="pnl">
  <div class="p-grid">
    <div class="p-stat"><b>{si?.today.search ?? 0}</b><span>今天搜索</span></div>
    <div class="p-stat"><b>{si?.today.fetch ?? 0}</b><span>今天读网页</span></div>
    <div class="p-stat"><b class:acc={ready} class:warn={si?.enabled && !ready}>{ready ? '可用' : si?.enabled ? '缺 key' : '关闭'}</b><span>状态</span></div>
  </div>
  <div class="p-card" class:warn={si?.enabled && !ready}>
    <div class="p-row"><span class="p-k">服务</span><span class="p-grow">DeepSeek 官方搜索接口</span></div>
    <div class="p-row"><span class="p-k">key</span><span class="p-grow">{keyText}</span></div>
    <p class="p-note">开着并且有 key 时，模型多两个工具：WebSearch 搜网页、WebFetch 读网页，回答时会附上来源链接；访客也能用。按次计费，每千次成功请求 5 元，空结果和失败不收费。工具定义在新会话、/reset 或整理上下文之后才出现在已有的会话里。</p>
    <button class="btn sm" disabled={!ready || testing} onclick={test}><Icon name="zap" size={13} />{testing ? '搜索中…' : '测试搜索（计费一次）'}</button>
  </div>
  <FieldGroup group="search" focus={store.focus} />
</div>
