<script lang="ts">
  // ＋ 接入机器：别的电脑装内核自己连进来（配对），或者网关去连 SSH 服务器
  import { onMount } from 'svelte';
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import { machineMeta } from '../../lib/format';
  import type { PairRequest } from '../../lib/types';
  import FieldGroup from '../FieldGroup.svelte';
  import SshForm from './SshForm.svelte';
  import Icon from '../Icon.svelte';

  const d = $derived(store.devices);
  const activeListener = $derived(d?.listener || (d?.primary_listener?.startsWith('tcp:') ? d.primary_listener.slice(4) : ''));
  const wanted = $derived(String(store.field('kernel.device_listen')?.value ?? ''));
  const base = $derived(d?.download_base || 'https://github.com/tofu-zyc/nano-agent/releases');
  // 网页在本机用 localhost 打开时，别的电脑连不到 127.0.0.1，换成网关的局域网地址
  const loopback = /^(localhost|127\.|\[?::1\]?$)/.test(location.hostname);
  const port = $derived((activeListener || '0.0.0.0:7700').split(':').pop());
  let gateway = $state('');
  $effect(() => {
    if (!gateway && d) gateway = (loopback && d.addrs?.length ? d.addrs[0] : location.hostname) + ':' + port;
  });
  const gwAddr = $derived(`tcp:${gateway || '网关地址:7700'}`);
  const commands = $derived({
    unix: `curl -fsSL ${base}/latest/download/install.sh | sh -s -- --kernel --gateway ${gwAddr}`,
    windows: `& ([scriptblock]::Create((irm ${base}/latest/download/install.ps1))) -Gateway ${gwAddr}`,
  });
  let copied = $state('');
  let busy = $state('');
  let adding = $state(false);

  onMount(() => {
    store.loadDevices();
    const t = setInterval(() => store.loadDevices(), 3000); // 配对请求、连接状态
    return () => clearInterval(t);
  });

  async function copy(which: 'unix' | 'windows') {
    try {
      await navigator.clipboard.writeText(commands[which]);
      copied = which;
      setTimeout(() => (copied = ''), 1600);
    } catch {
      store.toast({ kind: 'err', text: '复制失败，手动选中复制吧' }, 2400);
    }
  }
  async function enable() {
    busy = 'listener';
    try {
      await api.put('/api/devices/listener', { address: '0.0.0.0:7700' });
      await Promise.all([store.loadDevices(), store.loadFields()]);
      store.toast({ kind: 'ok', text: '电脑接入已启用（7700 端口）' });
    } catch (err) {
      store.fail(err);
    } finally {
      busy = '';
    }
  }
  async function decide(r: PairRequest, ok: boolean) {
    busy = r.id;
    await store.decidePair(r, ok);
    busy = '';
  }
</script>

<div class="pnl">
  {#each d?.pairing ?? [] as r (r.id)}
    <div class="p-card pair">
      <div class="p-row">
        <Icon name="laptop" size={16} />
        <strong class="p-grow">{r.node} 请求接入</strong>
        <span class="code">{r.code.slice(0, 3)} {r.code.slice(3)}</span>
      </div>
      <p class="p-note">来自 {r.addr}。核对那台电脑上显示的确认码，一致再允许。</p>
      <div class="p-actions tight">
        <button class="btn sm primary" disabled={busy === r.id} onclick={() => decide(r, true)}><Icon name="check" size={13} />允许接入</button>
        <button class="btn sm" disabled={busy === r.id} onclick={() => decide(r, false)}>拒绝</button>
      </div>
    </div>
  {/each}

  <h5 class="p-h"><Icon name="laptop" size={14} />别的电脑：装上内核，自己连进来</h5>
  <div class="p-card">
    <p class="p-note">Linux、macOS、Windows 都可以。一行命令下载安装包、校验、注册开机自启，然后连到这个网关请求接入，这里会出现确认码。</p>
    {#if !activeListener}
      <div class="p-warn">电脑接入还没启用：别的电脑要通过一个 TCP 端口连进来。</div>
      <button class="btn sm primary" disabled={!!busy} onclick={enable}>启用电脑接入 · 7700 端口</button>
    {:else if wanted && wanted.replace(/^tcp:/, '') !== activeListener}
      <div class="p-warn">设置的接入地址是 {wanted}，但现在监听的是 {activeListener}（地址用不了？看网关日志）。</div>
    {:else}
      <div class="p-row"><span class="dot online"></span><span class="p-grow">正在监听 {activeListener}</span></div>
    {/if}
    <label class="p-label">这台网关的地址（那台电脑能访问到的 IP 或域名）
      <input class="p-input" bind:value={gateway} list="gw-addrs" placeholder="192.168.1.2:7700" />
    </label>
    <datalist id="gw-addrs">{#each d?.addrs ?? [] as a}<option value={a + ':' + port}></option>{/each}</datalist>
    <div class="p-cmd"><span class="p-os">Linux / macOS</span><code>{commands.unix}</code><button class="btn ghost sm" onclick={() => copy('unix')} title="复制命令"><Icon name={copied === 'unix' ? 'check' : 'copy'} size={13} /></button></div>
    <div class="p-cmd"><span class="p-os">Windows</span><code>{commands.windows}</code><button class="btn ghost sm" onclick={() => copy('windows')} title="复制命令"><Icon name={copied === 'windows' ? 'check' : 'copy'} size={13} /></button></div>
    <p class="p-note p-small">在那台电脑上运行（Windows 在 PowerShell 里）。内核会自动换成和网关一样的版本；那台电脑上用 <code>nano-kernel status</code> 看状态，<code>nano-kernel connect 地址</code> 改连别的网关，<code>nano-kernel uninstall</code> 卸载。不能联网的电脑：把 Release 里对应的包拷过去，解压后运行里面的 install-kernel 脚本。</p>
  </div>

  <h5 class="p-h"><Icon name="server" size={14} />SSH 服务器：网关主动去连</h5>
  <div class="p-card">
    <p class="p-note">填主机、端口、用户，用密码或密钥；也可以直接选网关这台机器 <code>~/.ssh/config</code> 里的别名。服务器上不用装内核，网关自动放上执行器，掉线自动重连。</p>
    <button class="btn sm primary" onclick={() => (adding = true)}><Icon name="plus" size={13} />添加 SSH 服务器</button>
  </div>

  {#if store.machines.length}
    <h5 class="p-h">已经接入的机器 <span class="muted p-small">点一下看它的工作目录和连接</span></h5>
    <div class="p-card">
      {#each store.machines as m (m.name)}
        <button class="p-srow" onclick={() => store.open({ type: 'machine', name: m.name })}>
          <span class="dot {m.online ? 'online' : 'stopped'}"></span>
          <Icon name={machineMeta[m.kind].icon} size={14} />
          <span class="p-grow">{m.name}</span>
          <span class="tag">{m.kind === 'local' ? '网关所在的机器' : machineMeta[m.kind].label}</span>
          <Icon name="right" size={13} />
        </button>
      {/each}
    </div>
  {/if}

  <FieldGroup group="machines" focus={store.focus} />
</div>

{#if adding}
  <SshForm onclose={() => (adding = false)} />
{/if}

<style>
  .pair {
    border-color: color-mix(in srgb, var(--accent) 55%, var(--line));
    background: linear-gradient(100deg, color-mix(in srgb, var(--accent) 12%, var(--panel-2)), var(--panel-2));
    margin-bottom: 10px;
  }
  .code {
    font: 600 18px/1 var(--mono);
    letter-spacing: 0.12em;
    padding: 6px 10px;
    border-radius: 9px;
    background: var(--bg-2);
    border: 1px solid var(--line-2);
    white-space: nowrap;
  }
  .p-h :global(svg) {
    align-self: center;
    color: var(--c-kernel);
  }
</style>
