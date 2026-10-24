<script lang="ts">
  import { store } from '../../lib/state.svelte';
  import FieldGroup from '../FieldGroup.svelte';

  const mi = $derived(store.ov?.memory);
  function pctText(key: string, def: number) {
    const v = Number(store.field(key)?.value);
    return Math.round((v > 0 ? v : def) * 100) + '%';
  }
  const idle = $derived.by(() => {
    const v = String(store.field('context.idle_compact')?.value ?? '30m');
    if (v === '0' || v === '0s') return '空闲时不整理';
    const m = v.match(/^(\d+)(m|h)$/);
    return m ? `空闲 ${m[1]} ${m[2] === 'h' ? '小时' : '分钟'}后整理一次` : `空闲 ${v} 后整理一次`;
  });
</script>

<div class="pnl">
  <div class="p-grid">
    <div class="p-stat"><b>{pctText('context.compact_at', 0.75)}</b><span>到这里自动整理</span></div>
    <div class="p-stat"><b>{mi?.summarized ?? 0}</b><span>整理过的会话</span></div>
    <div class="p-stat"><b>{mi?.archives ?? 0}</b><span>归档</span></div>
  </div>
  <div class="p-card">
    <p class="p-note">每个会话的上下文到模型窗口的 {pctText('context.compact_at', 0.75)} 时自动整理，整理到 {pctText('context.compact_to', 0.4)}：先把旧的长工具输出换成一行占位，还不够再把更早的对话合成一段摘要，最近几轮原样保留。{idle}。聊天里发 <code>/context</code> 看用量，<code>/compact</code> 马上整理，<code>/reset</code> 把旧对话归档后清空；网页的对话顶部可以删历史和归档。</p>
    <p class="p-note">长期记忆（知识库、睡觉做梦）还没有做，做好以后也挂在这个节点上。</p>
  </div>
  <FieldGroup group="memory" focus={store.focus} />
</div>
