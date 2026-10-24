<script lang="ts">
  // 添加或编辑 SSH 服务器：主机（或 ~/.ssh/config 里的别名）、名字、用户、端口、密码或密钥
  import { untrack } from 'svelte';
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import { portal } from '../../lib/portal';
  import type { SshConnection } from '../../lib/types';
  import Icon from '../Icon.svelte';

  let { conn = null, onclose }: { conn?: SshConnection | null; onclose: () => void } = $props();

  const blank = { id: '', name: '', host: '', port: undefined as number | undefined, user: '', identity_file: '', password: '', has_password: false, clear_password: false, auto_connect: true };
  // 打开时的连接信息作为表单的初值（之后连接状态变了也不覆盖正在填的内容）
  const init = (c: SshConnection | null) => (c ? { ...blank, ...c, port: c.port || undefined, password: '', clear_password: false } : { ...blank });
  let form = $state(untrack(() => init(conn)));
  let busy = $state(false);
  let err = $state('');

  async function save(connect: boolean) {
    busy = true;
    err = '';
    try {
      const r = await api.post<{ id: string }>('/api/devices', { ...form, port: form.port ?? 0 });
      if (connect) await api.post(`/api/devices/${r.id}/connect`);
      await store.loadDevices();
      store.toast({ kind: 'ok', text: connect ? `已保存，正在连接 ${form.name}` : '已保存' }, 2400);
      onclose();
    } catch (e) {
      err = e instanceof Error ? e.message : String(e);
    } finally {
      busy = false;
    }
  }

  function onkey(e: KeyboardEvent) {
    if (e.key === 'Escape' && !e.isComposing) {
      e.preventDefault();
      onclose();
    }
  }
</script>

<svelte:window onkeydown={onkey} />

<div class="backdrop" use:portal onclick={(e) => e.target === e.currentTarget && onclose()} role="presentation">
  <form class="sheet" onsubmit={(e) => { e.preventDefault(); save(true); }}>
    <div class="top">
      <h3>{form.id ? '编辑 SSH 服务器' : '添加 SSH 服务器'}</h3>
      <button type="button" class="btn ghost sm" onclick={onclose} aria-label="关闭"><Icon name="x" size={15} /></button>
    </div>
    <p class="p-note">网关自己去连，服务器上不用装内核、不用开端口；连上后自动放上执行器（nano-exec），在会话里选这台服务器上的工作目录就能干活。SSH 配置里的密钥、跳板机、代理照常生效。</p>
    <div class="fields">
      <label class="p-label">主机地址或 SSH 别名<input class="p-input" required bind:value={form.host} list="ssh-hosts" placeholder="192.168.1.10 或 my-server" /></label>
      <label class="p-label">在会话里显示的名字<input class="p-input" required bind:value={form.name} placeholder="开发服务器" /></label>
      <label class="p-label">SSH 用户<input class="p-input" bind:value={form.user} placeholder="留空沿用 SSH 配置" autocomplete="off" /></label>
      <label class="p-label">端口<input class="p-input" type="number" min="1" max="65535" bind:value={form.port} placeholder="留空沿用 SSH 配置，通常是 22" /></label>
      <label class="p-label">密码<input class="p-input" type="password" bind:value={form.password} placeholder={form.has_password ? '已保存，留空保持' : '用密钥或 SSH 配置连接时留空'} autocomplete="new-password" /></label>
      <label class="p-label">密钥路径<input class="p-input" bind:value={form.identity_file} placeholder="可选，例如 ~/.ssh/id_ed25519" /></label>
    </div>
    <datalist id="ssh-hosts">{#each store.devices?.ssh_hosts ?? [] as h}<option value={h}></option>{/each}</datalist>
    <div class="opts">
      <label><input type="checkbox" bind:checked={form.auto_connect} />网关启动时连接，掉线后自动重连</label>
      {#if form.has_password}<label><input type="checkbox" bind:checked={form.clear_password} />清除已保存的密码</label>{/if}
    </div>
    {#if err}<div class="p-err">{err}</div>{/if}
    <div class="acts">
      <button type="button" class="btn" disabled={busy} onclick={() => save(false)}>只保存</button>
      <button type="submit" class="btn primary" disabled={busy}>{busy ? '正在保存…' : '保存并连接'}</button>
    </div>
  </form>
</div>

<style>
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
    max-height: 100%;
    overflow: auto;
    display: flex;
    flex-direction: column;
    gap: 14px;
    padding: 20px 22px;
    border-radius: 18px;
    border: 1px solid var(--line-2);
    background: var(--panel-solid);
    box-shadow: 0 24px 80px rgba(0, 0, 0, 0.4);
    animation: rise 0.2s ease;
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
  .fields {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 14px;
  }
  .opts {
    display: flex;
    flex-wrap: wrap;
    gap: 8px 18px;
    font-size: 12.5px;
    color: var(--text-2);
  }
  .opts label {
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .acts {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
  }
  @media (max-width: 600px) {
    .fields {
      grid-template-columns: 1fr;
    }
  }
</style>
