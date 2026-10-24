<script lang="ts">
  // ＋ 添加机器人：同一个 agent 可以有好几个机器人，一个机器人只绑一个 agent。
  // Telegram 要 Bot Token；飞书要 App ID 和 App Secret。
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import Icon from '../Icon.svelte';

  let kind = $state<'telegram' | 'feishu'>('telegram');
  let name = $state('');
  let agent = $state('default');
  let token = $state('');
  let appID = $state('');
  let appSecret = $state('');
  let proxy = $state('');
  let busy = $state(false);

  async function create() {
    busy = true;
    try {
      const body: any = { kind, name: name.trim(), agent, proxy: proxy.trim() };
      if (kind === 'telegram') body.token = token.trim();
      else {
        body.app_id = appID.trim();
        body.app_secret = appSecret.trim();
      }
      const r = await api.post<{ id: string }>('/api/bots', body);
      name = token = appID = appSecret = proxy = '';
      await store.refresh();
      store.inspector = { type: 'bot', id: r.id };
      store.toast({ kind: 'ok', text: '机器人加好了' }, 2400);
    } catch (err) {
      store.fail(err);
    } finally {
      busy = false;
    }
  }
</script>

<div class="pnl">
  <p class="p-note lead">机器人是 agent 对外的嘴：一个机器人只绑一个 agent，同一个 agent 可以有好几个机器人。内置的 Telegram、飞书两个机器人也能换绑。</p>

  <h5 class="p-h">加一个机器人</h5>
  <div class="p-card">
    <label class="p-label">种类
      <select class="p-select" bind:value={kind}>
        <option value="telegram">Telegram</option>
        <option value="feishu">飞书</option>
      </select>
    </label>
    <label class="p-label">名字
      <input class="p-input" bind:value={name} placeholder="比如 小鲍的 Telegram" />
    </label>
    <label class="p-label">绑给谁
      <select class="p-select" bind:value={agent}>
        {#each store.agents as a (a.id)}<option value={a.id}>{a.emoji} {a.name}</option>{/each}
      </select>
    </label>
    {#if kind === 'telegram'}
      <label class="p-label">Bot Token
        <input class="p-input" type="password" bind:value={token} placeholder="找 @BotFather 创建机器人拿到" />
      </label>
      <p class="p-note p-small">要在 BotFather 里关掉 privacy mode（或者让它当群管理员），才收得到群里所有消息。</p>
    {:else}
      <label class="p-label">App ID
        <input class="p-input" bind:value={appID} />
      </label>
      <label class="p-label">App Secret
        <input class="p-input" type="password" bind:value={appSecret} />
      </label>
      <p class="p-note p-small">飞书要开「获取群组中所有消息」权限，才收得到群消息。</p>
    {/if}
    <label class="p-label">代理（可以不填）
      <input class="p-input" bind:value={proxy} placeholder="留空用网关的默认代理" />
    </label>
    <div class="p-actions tight">
      <button class="btn sm primary" onclick={create} disabled={busy}><Icon name="plus" size={13} />添加</button>
    </div>
  </div>

  <h5 class="p-h">现在的机器人</h5>
  <div class="p-card">
    {#each store.bots as b (b.id)}
      <button class="p-srow" onclick={() => store.open({ type: 'bot', id: b.id })}>
        <Icon name={b.kind === 'feishu' ? 'feishu' : 'send'} size={14} />
        <span class="p-grow">{b.name}</span>
        <span class="muted p-small">{store.agent(b.agent)?.name ?? b.agent}</span>
        <Icon name="right" size={13} />
      </button>
    {/each}
  </div>
</div>
