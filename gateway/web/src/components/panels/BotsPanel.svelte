<script lang="ts">
  // ＋ 添加机器人：同一个 agent 可以有好几个机器人，一个机器人只绑一个 agent。
  // Telegram 要 Bot Token；飞书要 App ID 和 App Secret；QQ 扫码创建（也能手动填）；微信只能扫码连接。
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import { botKindMeta } from '../../lib/format';
  import Icon from '../Icon.svelte';
  import BotQR from '../BotQR.svelte';

  let kind = $state<'telegram' | 'feishu' | 'qq' | 'weixin'>('telegram');
  let manual = $state(false); // QQ：手动填 AppID 和 AppSecret
  let name = $state('');
  let agent = $state('default');
  let token = $state('');
  let appID = $state('');
  let appSecret = $state('');
  let proxy = $state('');
  let busy = $state(false);

  async function scanned(id: string) {
    name = proxy = '';
    await store.refresh();
    store.inspector = { type: 'bot', id };
    store.toast({ kind: 'ok', text: '机器人加好了' }, 2400);
  }

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
        <option value="qq">QQ</option>
        <option value="weixin">微信</option>
      </select>
    </label>
    <label class="p-label">名字
      <input class="p-input" bind:value={name} placeholder={kind === 'qq' ? '比如 小鲍的 QQ' : kind === 'weixin' ? '比如 我的微信' : '比如 小鲍的 Telegram'} />
    </label>
    <label class="p-label">绑给谁
      <select class="p-select" bind:value={agent}>
        {#each store.agents as a (a.id)}<option value={a.id}>{a.emoji} {a.name}</option>{/each}
      </select>
    </label>
    <label class="p-label">代理（可以不填）
      <input class="p-input" bind:value={proxy} placeholder="留空用网关的默认代理" />
    </label>
    {#if kind === 'telegram'}
      <label class="p-label">Bot Token
        <input class="p-input" type="password" bind:value={token} placeholder="找 @BotFather 创建机器人拿到" />
      </label>
      <p class="p-note p-small">要在 BotFather 里关掉 privacy mode（或者让它当群管理员），才收得到群里所有消息。</p>
    {:else if kind === 'weixin'}
      <BotQR kind="weixin" {name} {agent} {proxy} onDone={scanned} />
    {:else if kind === 'qq' && !manual}
      <BotQR kind="qq" {name} {agent} {proxy} onDone={scanned} />
      <button class="p-lnk p-small" onclick={() => (manual = true)}>已经有 AppID 和 AppSecret？手动填</button>
    {:else}
      <label class="p-label">App ID
        <input class="p-input" bind:value={appID} />
      </label>
      <label class="p-label">App Secret
        <input class="p-input" type="password" bind:value={appSecret} />
      </label>
      {#if kind === 'qq'}
        <p class="p-note p-small">在 <a href="https://q.qq.com/#/apps" target="_blank" rel="noopener">QQ 开放平台</a>的机器人「开发管理」里看；AppSecret 只在第一次能看到明文。<button class="p-lnk p-small" onclick={() => (manual = false)}>改成扫码</button></p>
      {:else}
        <p class="p-note p-small">飞书要开「获取群组中所有消息」权限，才收得到群消息。</p>
      {/if}
    {/if}
    {#if kind === 'telegram' || kind === 'feishu' || (kind === 'qq' && manual)}
      <div class="p-actions tight">
        <button class="btn sm primary" onclick={create} disabled={busy}><Icon name="plus" size={13} />添加</button>
      </div>
    {/if}
  </div>

  <h5 class="p-h">现在的机器人</h5>
  <div class="p-card">
    {#each store.bots as b (b.id)}
      <button class="p-srow" onclick={() => store.open({ type: 'bot', id: b.id })}>
        <Icon name={botKindMeta[b.kind]?.icon ?? 'send'} size={14} />
        <span class="p-grow">{b.name}</span>
        <span class="muted p-small">{store.agent(b.agent)?.name ?? b.agent}</span>
        <Icon name="right" size={13} />
      </button>
    {/each}
  </div>
</div>
