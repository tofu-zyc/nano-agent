<script lang="ts">
  // 仓库工作目录上的 GitHub 连接（设计 §16.3）：保存时用 token 查账号和仓库访问，不通就不保存；token 和密钥不回显。
  import { store } from '../lib/state.svelte';
  import { api } from '../lib/api';

  let { workspace }: { workspace: string } = $props();

  type RepoView = {
    id: string; workspace: string; owner: string; name: string; base: string; agent?: string; bot: string; intake: string; interval: number;
    roster: string[]; check?: string; budget?: number; token_set: boolean; secret_set: boolean; webhook_path?: string;
  };
  let items = $state<RepoView[]>([]);
  const mine = $derived(items.find((r) => r.workspace === workspace));
  let editing = $state(false);
  let busy = $state(false);
  let error = $state('');
  let f = $state({ owner: '', name: '', base: '', token: '', intake: 'poll', interval: 120, secret: '', roster: '', agent: '', check: '', budget: 0 });

  async function load() {
    try {
      items = (await api.get<{ items: RepoView[] }>('/api/repos')).items ?? [];
    } catch {
      /* 下次再读 */
    }
  }
  $effect(() => {
    workspace;
    void load();
  });
  function edit() {
    const r = mine;
    f = {
      owner: r?.owner ?? '', name: r?.name ?? '', base: r?.base ?? '', token: '', intake: r?.intake ?? 'poll', interval: r?.interval ?? 120,
      secret: '', roster: (r?.roster ?? []).join('\n'), agent: r?.agent ?? '', check: r?.check ?? '', budget: r?.budget ?? 0,
    };
    error = '';
    editing = true;
  }
  async function save() {
    busy = true;
    error = '';
    try {
      const roster = f.roster.split(/[\s,]+/).map((s) => s.trim()).filter(Boolean);
      await api.post('/api/repos', {
        id: mine?.id ?? '', workspace, owner: f.owner.trim(), name: f.name.trim(), base: f.base.trim(), agent: f.agent.trim(), token: f.token,
        intake: f.intake, interval: Number(f.interval) || 0, secret: f.secret, roster, check: f.check, budget: Number(f.budget) || 0,
      });
      f.token = '';
      f.secret = '';
      editing = false;
      await load();
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      busy = false;
    }
  }
  async function remove() {
    if (!mine || !confirm(`断开 ${mine.owner}/${mine.name}？之后这个工作目录的工单不再走 PR。`)) return;
    try {
      await api.del(`/api/repos/${mine.id}`);
      await load();
    } catch (err) {
      store.fail(err);
    }
  }
  const webhookURL = $derived(mine?.webhook_path ? location.origin + mine.webhook_path : '');
</script>

<div class="p-card" data-testid="repo-connect">
  <div class="p-row"><strong class="p-grow">GitHub 连接</strong></div>
  {#if mine && !editing}
    <p class="p-note" data-testid="repo-summary">
      已连接 <code>{mine.owner}/{mine.name}</code>，机器人账号 <code>{mine.bot}</code>，事件靠{mine.intake === 'webhook' ? ' webhook' : `轮询（每 ${mine.interval} 秒）`}。
      这个工作目录的工单都走 PR 交付，议题里名单上的人 @{mine.bot} 就能派活。
    </p>
    {#if webhookURL}
      <p class="p-note p-small">在 GitHub 仓库的 Webhooks 里填：<code data-testid="repo-webhook-url">{webhookURL}</code>，内容类型 JSON，密钥填你设的那个；网关要能从公网访问。事件勾选 Issues、Issue comments、Pull requests、Pull request reviews、Pull request review comments、Check suites。</p>
    {/if}
    <div class="p-actions tight">
      <button class="btn sm" onclick={edit} data-testid="repo-edit">修改</button>
      <button class="btn sm" onclick={remove} data-testid="repo-remove">断开</button>
    </div>
  {:else if !editing}
    <p class="p-note">连上 GitHub 仓库后，议题和 PR 是对话，这个工作目录的工单走 PR 交付。要用一个单独的 GitHub 账号做机器人：用你自己的账号时，你自己的评论会被当成机器人发的而忽略。token 用细粒度的：内容、PR、议题读写，检查只读。</p>
    <div class="p-actions tight"><button class="btn sm primary" onclick={edit} data-testid="repo-connect-btn">连接 GitHub 仓库</button></div>
  {/if}
  {#if editing}
    <label class="p-label">仓库的 owner
      <input class="p-input" bind:value={f.owner} data-testid="repo-owner" placeholder="acme" /></label>
    <label class="p-label">仓库名
      <input class="p-input" bind:value={f.name} data-testid="repo-name" placeholder="app" /></label>
    <label class="p-label">默认分支（空：问 GitHub）
      <input class="p-input" bind:value={f.base} placeholder="main" /></label>
    <label class="p-label">token{mine?.token_set ? '（已设置，留空不改）' : ''}
      <input class="p-input" type="password" autocomplete="off" bind:value={f.token} data-testid="repo-token" /></label>
    <label class="p-label">事件怎么进来
      <select class="p-select" bind:value={f.intake} data-testid="repo-intake">
        <option value="poll">轮询（不用公网地址）</option>
        <option value="webhook">webhook（要配签名密钥，网关要能从公网访问）</option>
      </select></label>
    {#if f.intake === 'poll'}
      <label class="p-label">轮询间隔（秒，最少 60）
        <input class="p-input" inputmode="numeric" bind:value={f.interval} /></label>
    {:else}
      <label class="p-label">webhook 的签名密钥{mine?.secret_set ? '（已设置，留空不改）' : ''}
        <input class="p-input" type="password" autocomplete="off" bind:value={f.secret} data-testid="repo-secret" /></label>
    {/if}
    <label class="p-label">准入名单（能派活的 GitHub 账号，写 gh:账号，空格或换行分开；管理员不用写，本来就能派）
      <textarea class="p-input" rows="3" bind:value={f.roster} data-testid="repo-roster" placeholder="gh:alice"></textarea></label>
    <p class="p-note p-small">议题正文的作者不在名单里时，这个议题建的工单里的外部 agent 也是每次都要你批准。</p>
    <label class="p-label">验收命令（在工单的工作区里跑；空表示只看有没有改动）
      <input class="p-input" bind:value={f.check} /></label>
    <label class="p-label">回答议题的 agent（空是默认 agent）
      <input class="p-input" bind:value={f.agent} /></label>
    {#if error}<p class="p-warn" data-testid="repo-error">{error}</p>{/if}
    <div class="p-actions tight">
      <button class="btn sm primary" disabled={busy} onclick={save} data-testid="repo-save">保存（会先用 token 查一次）</button>
      <button class="btn sm" onclick={() => (editing = false)}>取消</button>
    </div>
  {/if}
</div>
