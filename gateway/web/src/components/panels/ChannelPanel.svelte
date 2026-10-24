<script lang="ts">
  import { store } from '../../lib/state.svelte';
  import { api } from '../../lib/api';
  import { ago, channelMeta, stateLabel } from '../../lib/format';
  import type { EndpointInfo } from '../../lib/types';
  import FieldGroup from '../FieldGroup.svelte';
  import FeishuQR from '../FeishuQR.svelte';
  import Icon from '../Icon.svelte';

  let { name }: { name: string } = $props();

  const ov = $derived(store.ov!);
  const c = $derived(ov.channels.find((x) => x.name === name));
  // 在这个渠道上聊过天的人（每个私聊是一个窗口）
  const people = $derived((ov.sessions ?? []).flatMap((s) => (s.endpoints ?? []).filter((e) => e.channel === name).map((e) => ({ e, s }))));
  const showDefault = $derived(String(store.field('channels.show')?.value ?? '过程'));
  // 内置的 Telegram / 飞书也是一个机器人，绑在某个 agent 上（设置的“家”仍然在这个节点）
  const bot = $derived(name === 'telegram' ? store.botOfChannel('tg') : name === 'feishu' ? store.botOfChannel('feishu') : undefined);

  async function bindAgent(agent: string) {
    if (!bot) return;
    try {
      await api.patch('/api/bots/' + encodeURIComponent(bot.id), { agent });
      await store.refresh();
      store.toast({ kind: 'ok', text: '换好了，新消息进新 agent 的会话' }, 2600);
    } catch (err) {
      store.fail(err);
    }
  }

  async function moveEndpoint(e: EndpointInfo, sid: string) {
    try {
      await api.put(`/api/endpoints/${encodeURIComponent(e.key)}`, { session: sid });
      store.toast({ kind: 'ok', text: `${e.name || e.key} 已接到主会话` }, 2400);
    } catch (err) {
      store.fail(err);
    }
  }
  // 把在这个窗口说话的人设为管理员：写进管理员名单，窗口接到主会话
  async function makeAdmin(e: EndpointInfo) {
    const who = e.name ? `${e.name}（${e.who}）` : e.who;
    if (!confirm(`把 ${who} 设为管理员？\n管理员能让机器人在工作目录里读写文件、执行命令，能看到和切换所有会话。这个窗口会接到主会话。`)) return;
    try {
      await api.post(`/api/endpoints/${encodeURIComponent(e.key)}/admin`);
      store.toast({ kind: 'ok', text: `${e.name || e.who} 现在是管理员了，窗口已接到主会话` }, 3000);
    } catch (err) {
      store.fail(err);
    }
  }
</script>

<div class="pnl">
  {#if c}
    <div class="p-card" class:bad={c.status.state === 'error'}>
      <div class="p-row">
        <span class="dot {c.status.state}"></span>
        <strong class="p-grow">{stateLabel[c.status.state] ?? c.status.state}</strong>
        {#if c.status.since}<span class="muted p-small">{ago(c.status.since)}</span>{/if}
      </div>
      {#if c.status.identity}<div class="p-row"><span class="p-k">机器人</span><span class="p-grow">{c.status.identity}</span></div>{/if}
      {#if c.status.error}<div class="p-err">{c.status.error}</div>{/if}
      {#if name === 'web'}
        <p class="p-note">网页能看到所有会话，也能往任何一个会话里发消息，网页用户都是管理员。</p>
        <a class="btn sm primary" href="#/sessions/main" onclick={() => (store.inspector = null)}><Icon name="chat" size={14} />打开主会话</a>
      {:else if name === 'cli'}
        <p class="p-note">
          在另一个终端运行 <code>make chat</code>（或 <code>nano-gateway chat</code>）就能和网关聊天，随时进出，网关照常运行；连上时会先显示最近的对话。
          终端的身份是 <code>cli:local</code>，始终是管理员，默认接在主会话上（用 <code>/session</code> 切换）。同一个会话里别的窗口说的话和回复，终端里也会同步显示。
        </p>
      {:else}
        <p class="p-note">私聊机器人的每个人是一个窗口。管理员默认接在主会话上，和终端、网页共享上下文；其他人各自一个会话。管理员可以发 <code>/session</code> 切换。</p>
      {/if}
      {#if name !== 'web'}
        <p class="p-note p-small">聊天里显示多少过程：默认跟随 Agent（现在是「{showDefault}」），下面可以给{channelMeta[name]?.label ?? name}单独选；某个聊天窗口还可以发 <code>/show</code>。</p>
      {/if}
    </div>
  {/if}

  {#if bot}
    <h5 class="p-h">这个机器人是谁</h5>
    <div class="p-card">
      <label class="p-label">绑定的 agent
        <select class="p-select" value={bot.agent} onchange={(e) => bindAgent((e.target as HTMLSelectElement).value)}>
          {#each store.agents as a (a.id)}<option value={a.id}>{a.emoji} {a.name}{a.default ? '（默认）' : ''}</option>{/each}
        </select>
      </label>
      <p class="p-note p-small">一个机器人只绑一个 agent：它上面的消息都进这个 agent 的会话。想给别的 agent 一个 Telegram，可以在拓扑图里「添加机器人」。</p>
    </div>
  {/if}

  {#if name === 'telegram' || name === 'feishu'}
    {#if people.length}
      <h5 class="p-h">聊过天的人 <span class="muted p-small">每个私聊是一个窗口</span></h5>
      <div class="people">
        {#each people as { e, s } (e.key)}
          <div class="person" class:guest={!e.admin}>
            <div class="p-row">
              <strong class="p-grow">{e.name || e.key}</strong>
              {#if e.admin}<span class="tag accent">管理员</span>{:else}<span class="tag">访客</span>{/if}
            </div>
            <div class="where">
              接在<button class="p-lnk" onclick={() => store.open({ type: 'session', sid: s.sid })}>「{s.title}」</button>
              {#if e.who}<code>{e.who}</code>{/if}
            </div>
            {#if !e.admin && !e.who}
              <p class="p-note">还不知道这个窗口里是谁在说话（升级前的记录）。在{channelMeta[e.channel]?.label ?? ''}里给机器人再发一条消息，这里就能设为管理员。</p>
            {:else if !e.admin}
              <p class="p-note">访客只能聊天，不能用工具，也看不到别的会话。如果这是你自己，设为管理员后这个窗口会接到主会话，和终端、网页共用上下文。</p>
              <button class="btn sm primary" onclick={() => makeAdmin(e)}><Icon name="shield" size={13} />设为管理员并接到主会话</button>
            {:else if e.session !== 'main'}
              <button class="btn sm" onclick={() => moveEndpoint(e, 'main')}><Icon name="hub" size={13} />接到主会话</button>
            {/if}
          </div>
        {/each}
      </div>
    {:else if c?.status.state === 'online'}
      <p class="p-note" style="margin-top: 12px">还没有人私聊过机器人。私聊它说一句话，这里就会出现，可以把你自己设为管理员。</p>
    {/if}
  {/if}

  {#if name === 'telegram' && c?.status.state === 'disabled'}
    <details class="p-guide" open>
      <summary>怎么接入 Telegram</summary>
      <ol>
        <li>找 <a href="https://t.me/BotFather" target="_blank" rel="noopener">@BotFather</a> 发 <code>/newbot</code>，拿到 token，填到下面；</li>
        <li>国内一般要设代理，比如 <code>http://127.0.0.1:7890</code>（或者在网关节点设默认代理）；</li>
        <li>打开“启用”，几秒后这里会显示机器人的名字；</li>
        <li>私聊机器人就能用了。要让它看得见群里的所有消息：在 BotFather 里 <code>/setprivacy</code> 关掉 privacy mode，或者把它设成群管理员；不然它只收得到 @ 它的消息。</li>
      </ol>
    </details>
  {/if}
  {#if name === 'feishu'}
    <FeishuQR configured={!!store.field('feishu.app_id')?.value} />
    <details class="p-guide">
      <summary>或者手动接入</summary>
      <ol>
        <li>在 <a href="https://open.feishu.cn/app" target="_blank" rel="noopener">飞书开放平台</a> 创建企业自建应用，添加“机器人”能力；</li>
        <li>把 App ID 和 App Secret 填到下面，打开“启用”；</li>
        <li>在“事件与回调”里选“使用长连接接收事件”，添加 <code>im.message.receive_v1</code>、<code>im.message.recalled_v1</code>；</li>
        <li>“回调配置”里同样选长连接，添加 <code>card.action.trigger</code>（卡片上的停止、批准 / 拒绝按钮要用）；</li>
        <li>开通 <code>im:message</code>、<code>im:message:send_as_bot</code>、<code>cardkit:card:write</code> 权限，发布版本；</li>
        <li>要在群里用：再开“获取群组中所有消息”的权限，不然只收得到 @ 它的消息。</li>
      </ol>
    </details>
  {/if}

  <FieldGroup group={name} focus={store.focus} />
</div>

<style>
  .people {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .person {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 6px;
    padding: 11px 13px;
    border-radius: 13px;
    border: 1px solid var(--line);
    background: var(--panel-2);
  }
  .person .p-row {
    width: 100%;
  }
  .person.guest {
    border-color: color-mix(in srgb, var(--warn) 35%, var(--line));
  }
  .where {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    color: var(--muted);
  }
  .where code {
    font-size: 11.5px;
    word-break: break-all;
  }
</style>
