<script lang="ts">
  // 外部 agent（设计 §16.4）：主人登记命令模板（Claude Code、Codex 这样的命令行程序），在工作目录上显式开启哪些能用，默认都不开。
  import { store } from '../lib/state.svelte';
  import { api } from '../lib/api';
  import type { WorkspaceInfo } from '../lib/types';

  let { ws }: { ws: WorkspaceInfo } = $props();

  type Agent = { name: string; argv: string[]; dirs: string[]; env?: Record<string, string>; timeout?: number };
  let items = $state<Agent[]>([]);
  let editing = $state(false);
  let busy = $state(false);
  let error = $state('');
  let f = $state({ name: '', argv: '', dirs: '', env: '', timeout: 0 });
  const enabled = $derived(ws.external ?? []);

  async function load() {
    try {
      items = (await api.get<{ items: Agent[] }>('/api/external-agents')).items ?? [];
    } catch {
      /* 下次再读 */
    }
  }
  $effect(() => {
    ws.id;
    void load();
  });
  async function toggle(name: string, on: boolean) {
    if (on && !confirm(`在这个工作目录开启 ${name}？\n\n这是对沙箱的放宽：开启后，${name} 能读写自己登记的目录（里面有它的登录凭据），也能联网。只在名单里的人派的工单和管理员的会话里能用，每次调用跟着权限模式审批。`)) return;
    const next = on ? [...enabled.filter((n) => n !== name), name] : enabled.filter((n) => n !== name);
    try {
      await api.patch('/api/workspaces/' + ws.id, { external: next });
      await store.refresh();
    } catch (err) {
      store.fail(err);
    }
  }
  function edit(a?: Agent) {
    f = {
      name: a?.name ?? '', argv: (a?.argv ?? ['claude', '-p', '{prompt}']).join('\n'), dirs: (a?.dirs ?? []).join('\n'),
      env: Object.entries(a?.env ?? {}).map(([k, v]) => `${k}=${v}`).join('\n'), timeout: a?.timeout ?? 0,
    };
    error = '';
    editing = true;
  }
  const lines = (s: string) => s.split('\n').map((x) => x.trim()).filter(Boolean);
  const sortEnv = (env: Record<string, string>) => Object.fromEntries(Object.entries(env).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)));
  async function save() {
    busy = true;
    error = '';
    try {
      const env: Record<string, string> = {};
      for (const l of lines(f.env)) {
        const i = l.indexOf('=');
        if (i <= 0) throw new Error(`env 这一行要写成 名字=值：${l}`);
        env[l.slice(0, i).trim()] = l.slice(i + 1).trim();
      }
      const next = { name: f.name.trim(), argv: f.argv.split('\n').filter((x) => x !== ''), dirs: lines(f.dirs), env, timeout: Number(f.timeout) || 0 };
      // 改了命令、目录或环境（或者新登记的名字），所有开着它的工作目录都会关掉：开启时确认的是那时候登记的目录
      const prev = items.find((x) => x.name === next.name);
      const same = prev && JSON.stringify([prev.argv, prev.dirs, prev.env ?? {}]) === JSON.stringify([next.argv, next.dirs, sortEnv(env)]);
      const affected = same ? [] : (store.ov?.workspaces ?? []).filter((w) => w.external?.includes(next.name));
      if (affected.length && !confirm(`保存后，这些工作目录上的 ${next.name} 会关掉，要重新开启、重新确认：\n\n${affected.map((w) => `${w.host}:${w.path}`).join('\n')}`)) return;
      await api.post('/api/external-agents', next);
      editing = false;
      await load();
      await store.refresh();
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      busy = false;
    }
  }
  async function remove(a: Agent) {
    if (!confirm(`删掉外部 agent ${a.name} 的登记？所有工作目录上都会关掉它。`)) return;
    try {
      await api.del('/api/external-agents/' + encodeURIComponent(a.name));
      await load();
      await store.refresh();
    } catch (err) {
      store.fail(err);
    }
  }
</script>

<div class="p-card" data-testid="external-agents">
  <div class="p-row"><strong class="p-grow">外部 agent</strong></div>
  <p class="p-note">Claude Code、Codex 这样的命令行程序登记成命令就能用：在这个工作目录（和它的工单）里作为后台任务跑，改完照常验收。默认都不开。</p>
  {#each items as a (a.name)}
    <div class="p-row">
      <label class="p-grow"><input type="checkbox" checked={enabled.includes(a.name)} onchange={(e) => toggle(a.name, e.currentTarget.checked)} data-testid="ext-toggle-{a.name}" />
        <code>{a.name}</code> <span class="muted p-small">{a.argv.join(' ')}</span></label>
      <button class="btn sm" onclick={() => edit(a)}>修改</button>
      <button class="btn sm" onclick={() => remove(a)}>删掉</button>
    </div>
    {#if enabled.includes(a.name)}<p class="p-warn p-small" data-testid="ext-on-{a.name}">已开启：{a.name} 能读写 {a.dirs.join('、') || '（没有登记目录）'}，也能联网。</p>{/if}
  {:else}
    <p class="p-note">还没有登记外部 agent。</p>
  {/each}
  {#if editing}
    <label class="p-label">名字（小写字母开头，比如 claude、codex）
      <input class="p-input" bind:value={f.name} data-testid="ext-name" /></label>
    <label class="p-label">命令，一行一项；{'{prompt}'} 单独占一行，换成这次的要求（不经 shell 解释）
      <textarea class="p-input" rows="4" bind:value={f.argv} data-testid="ext-argv"></textarea></label>
    <label class="p-label">它自己的配置和登录目录，一行一个，以 ~/ 开头。装在家目录里的程序，登记它的安装目录（比如 ~/.local/share/claude）；
      不要登记 PATH 里放启动脚本的目录（比如 ~/.local/bin）：那里的程序主人自己的终端和宿主之后的步骤都会去跑，不能让外部 agent 改，和它互相包含的目录执行时会被拒绝
      <textarea class="p-input" rows="2" bind:value={f.dirs} data-testid="ext-dirs" placeholder="~/.claude"></textarea></label>
    <label class="p-label">环境变量，一行一个 名字=值：放路径和开关，不放密钥（HOME、PATH 这些改不了）
      <textarea class="p-input" rows="2" bind:value={f.env} data-testid="ext-env" placeholder="CLAUDE_CONFIG_DIR=~/.claude"></textarea></label>
    <label class="p-label">时限（秒，0 是默认 1800）
      <input class="p-input" inputmode="numeric" bind:value={f.timeout} /></label>
    {#if error}<p class="p-warn" data-testid="ext-error">{error}</p>{/if}
    <div class="p-actions tight">
      <button class="btn sm primary" disabled={busy} onclick={save} data-testid="ext-save">登记</button>
      <button class="btn sm" onclick={() => (editing = false)}>取消</button>
    </div>
  {:else}
    <div class="p-actions tight"><button class="btn sm" onclick={() => edit()} data-testid="ext-add">登记外部 agent</button></div>
  {/if}
</div>
