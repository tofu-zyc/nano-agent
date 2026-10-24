<script lang="ts">
  // 一个机器人节点：状态 → 操作（绑定 agent）→ 设置（凭据、代理、显示档位）→ 危险操作。
  // 内置的 tg / feishu 的凭据在 Telegram / 飞书节点里（设置只有一个家），这里只管绑定和开关。
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import { stateLabel } from '../../lib/format';
  import Icon from '../Icon.svelte';

  let { id }: { id: string } = $props();

  const b = $derived(store.bots.find((x) => x.id === id));
  const ov = $derived(store.ov!);
  const channel = $derived(ov.channels.find((c) => c.name === b?.channel));
  let name = $state('');
  let token = $state('');
  let secret = $state('');

  async function patch(body: any) {
    try {
      await api.patch('/api/bots/' + encodeURIComponent(id), body);
      await store.refresh();
      store.toast({ kind: 'ok', text: '已保存' }, 1800);
    } catch (err) {
      store.fail(err);
    }
  }

  async function setCreds() {
    const body: any = {};
    if (token.trim()) body.token = token.trim();
    if (secret.trim()) body.app_secret = secret.trim();
    if (!Object.keys(body).length) return;
    await patch(body);
    token = '';
    secret = '';
  }

  async function remove() {
    if (!confirm('删掉这个机器人？它上面的聊天记录还在。')) return;
    try {
      await api.del('/api/bots/' + encodeURIComponent(id));
      store.inspector = null;
      await store.refresh();
    } catch (err) {
      store.fail(err);
    }
  }
</script>

{#if b}
  <div class="pnl">
    <div class="p-card" class:bad={channel?.status.state === 'error'}>
      <div class="p-row">
        <span class="dot {channel?.status.state ?? 'disabled'}"></span>
        <strong class="p-grow">{stateLabel[channel?.status.state ?? 'disabled'] ?? '停用了'}</strong>
        {#if channel?.status.identity}<span class="muted p-small">{channel.status.identity}</span>{/if}
      </div>
      {#if channel?.status.error}<div class="p-err">{channel.status.error}</div>{/if}
      <div class="p-row"><span class="p-k">渠道</span><code class="p-grow">{b.channel}</code></div>
      <div class="p-row"><span class="p-k">种类</span><span class="p-grow">{b.kind === 'feishu' ? '飞书' : 'Telegram'}{#if b.legacy}<span class="tag">内置</span>{/if}</span></div>
    </div>

    <h5 class="p-h">绑定</h5>
    <div class="p-card">
      <label class="p-label">这个机器人是谁
        <select class="p-select" value={b.agent} onchange={(e) => patch({ agent: (e.target as HTMLSelectElement).value })}>
          {#each store.agents as a (a.id)}<option value={a.id}>{a.emoji} {a.name}</option>{/each}
        </select>
      </label>
      <p class="p-note p-small">换绑以后，这个机器人上的新消息进新 agent 的会话；以前的会话留在原来的 agent 下面。</p>
      <label class="p-label">聊天里显示多少过程
        <select class="p-select" value={b.show} onchange={(e) => patch({ show: (e.target as HTMLSelectElement).value })}>
          <option value="">跟随 agent</option>
          <option value="详细">详细</option>
          <option value="过程">过程</option>
          <option value="简洁">简洁</option>
        </select>
      </label>
      <label class="p-label">启用
        <select class="p-select" value={b.enabled ? '1' : ''} onchange={(e) => patch({ enabled: (e.target as HTMLSelectElement).value === '1' })}>
          <option value="1">启用</option>
          <option value="">停用</option>
        </select>
      </label>
      <label class="p-label">名字
        <input class="p-input" value={b.name} onchange={(e) => patch({ name: (e.target as HTMLInputElement).value })} />
      </label>
    </div>

    {#if b.legacy}
      <h5 class="p-h">凭据 <span class="muted p-small">在 {b.kind === 'feishu' ? '飞书' : 'Telegram'} 节点里改</span></h5>
      <div class="p-card">
        <button class="btn sm" onclick={() => store.open({ type: 'channel', name: b!.kind === 'feishu' ? 'feishu' : 'telegram' })}>
          <Icon name="arrow" size={13} />去{b.kind === 'feishu' ? '飞书' : 'Telegram'}节点
        </button>
      </div>
    {:else}
      <h5 class="p-h">凭据 <span class="muted p-small">加密保存，网页不回显</span></h5>
      <div class="p-card">
        {#if b.kind === 'telegram'}
          <label class="p-label">Bot Token {b.has_token ? '（已经设置，留空不改）' : ''}
            <input class="p-input" type="password" bind:value={token} placeholder="找 @BotFather 要" />
          </label>
        {:else}
          <label class="p-label">App ID
            <input class="p-input" value={b.app_id} onchange={(e) => patch({ app_id: (e.target as HTMLInputElement).value })} />
          </label>
          <label class="p-label">App Secret {b.has_secret ? '（已经设置，留空不改）' : ''}
            <input class="p-input" type="password" bind:value={secret} />
          </label>
        {/if}
        <label class="p-label">代理
          <input class="p-input" value={b.proxy} placeholder="留空用默认代理" onchange={(e) => patch({ proxy: (e.target as HTMLInputElement).value })} />
        </label>
        <div class="p-actions tight">
          <button class="btn sm primary" onclick={setCreds}>保存凭据</button>
        </div>
      </div>
      <div class="p-danger-zone">
        <button class="btn sm danger" onclick={remove}><Icon name="trash" size={13} />删掉这个机器人</button>
      </div>
    {/if}
  </div>
{:else}
  <p class="p-note">没有这个机器人。</p>
{/if}
