<script lang="ts">
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import { osLabel, since } from '../../lib/format';
  import Icon from '../Icon.svelte';

  let { node }: { node?: string } = $props();

  const ov = $derived(store.ov!);
  const k = $derived(node ? ov.kernels.find((x) => x.node === node) : undefined);
  const busy = $derived(k ? (ov.sessions ?? []).filter((s) => s.run_node === k.node) : []);
  const machine = $derived(k ? (k.local ? '本机' : k.node) : '');
  const maxJobSeconds = $derived(k?.features?.job_timeout_max_seconds ?? 0);
  const paired = $derived(!!k && !k.local && (store.devices?.paired ?? []).some((p) => p.node === k.node));

  async function unpair() {
    if (!k || !confirm(`移除 ${k.node}？它的口令会作废，现在就会断开。要再接入，需要重新配对。`)) return;
    try {
      await api.del(`/api/paired/${encodeURIComponent(k.node)}`);
      store.toast({ kind: 'ok', text: '已移除' });
      store.loadDevices();
    } catch (err) {
      store.fail(err);
    }
  }
</script>

<div class="pnl">
  {#if k}
    <div class="p-card">
      <div class="p-row">
        <span class="dot online"></span>
        <strong class="p-grow">{k.node}</strong>
        <span class="tag">{k.managed ? '网关启动' : k.local ? '在本机' : '在别的电脑上'}</span>
        {#if k.landlock > 0}<span class="tag"><Icon name="shield" size={11} />支持 Landlock v{k.landlock}</span>{:else}<span class="tag warn">没有完整隔离能力</span>{/if}
      </div>
      <div class="p-row"><span class="p-k">节点部署</span><span class="p-grow">{k.sandbox_mode || 'auto'} · 探测状态 {k.sandbox_effective || '未报告'}</span></div>
      <p class="p-note">这里是能力探测。实际执行隔离按每次调用结果显示；请求 on 与节点 off 冲突时失败。</p>
      <div class="p-row"><span class="p-k">系统</span><span class="p-grow">{[osLabel(k.os), k.arch, k.version ? 'v' + k.version : ''].filter(Boolean).join(' · ') || '—'}</span></div>
      <div class="p-row"><span class="p-k">连接</span><span class="p-grow">已连接 {since(k.since)}</span></div>
      {#if (k.features?.job_stream ?? 0) >= 1 && maxJobSeconds > 0}
        <div class="p-row"><span class="p-k">长命令上限</span><span class="p-grow">{k.features?.job_allow_no_timeout ? '允许不限时，需显式请求' : '最长 ' + maxJobSeconds.toLocaleString() + ' 秒'}</span></div>
        <p class="p-note">这是执行节点的部署上限，请求不能提高它；对话自身的时限仍单独生效。</p>
      {/if}
      <div class="p-row"><span class="p-k">工具</span><span class="p-grow">{k.tools.join('、')}</span></div>
      <div class="p-row"><span class="p-k">正在跑</span><span class="p-grow">{busy.length ? busy.map((s) => s.title).join('、') : '没有'}</span></div>
      <button class="btn sm" onclick={() => store.open({ type: 'machine', name: machine })}><Icon name="hub" size={13} />它所在的机器：{machine}</button>
    </div>
    <p class="p-note" style="margin-top: 10px">内核是跑对话循环、判断权限的程序，节点部署要求在 kernel.conf 设置；工具调用轮数上限、每条消息的时限这些在 <button class="p-lnk" onclick={() => store.open({ type: 'agentDefaults' }, 'agent.max_steps')}>Agent 默认值</button>里设，网关每一轮带给它；它连哪个网关写在那台电脑的 kernel.conf 里（<code>nano-kernel connect 地址</code> 改）。版本自动跟网关一致。</p>
    {#if paired}
      <div class="p-danger-zone">
        <button class="btn sm danger" onclick={unpair}><Icon name="trash" size={13} />移除这台电脑（口令作废）</button>
      </div>
    {/if}
  {:else if node}
    <p class="p-note">内核 {node} 现在不在线。</p>
  {:else}
    <div class="p-card bad">
      <strong>没有内核在线</strong>
      <p class="p-note">网关会在本机自己启动一个内核（本机节点里的「启动本机内核」开着时）；它起不来时看网关日志。别的电脑装上内核后自己连进来。</p>
      <button class="btn sm primary" onclick={() => store.open({ type: 'machines' })}><Icon name="plus" size={13} />接入电脑</button>
    </div>
  {/if}
</div>
