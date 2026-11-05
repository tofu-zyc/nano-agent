<script lang="ts">
  // 内部群的场内编排：发言顺序和轮数、私下频道、导演的“让 TA 说”、旁白输入框。
  import { api } from '../lib/api';
  import { store } from '../lib/state.svelte';
  import Icon from './Icon.svelte';
  import type { RoomInfo } from '../lib/types';

  let { r, onsave, desk = false }: { r: RoomInfo; onsave: (p: Record<string, unknown>) => Promise<void> | void; desk?: boolean } = $props();

  const name = (id: string) => store.agents.find((a) => a.id === id)?.name ?? id;
  const order = $derived(r.order?.length ? r.order.filter((m) => r.members.includes(m)) : r.members);
  const channels = $derived(r.channels ?? []);

  function move(i: number, d: number) {
    const o = [...order];
    const j = i + d;
    if (j < 0 || j >= o.length) return;
    [o[i], o[j]] = [o[j], o[i]];
    onsave({ order: o });
  }

  // 频道
  let newName = $state('');
  function addChannel() {
    const n = newName.trim();
    if (!n) return;
    const id = 'c' + Math.random().toString(36).slice(2, 8);
    onsave({ channels: [...channels, { id, name: n, members: r.members.slice(0, 2) }] });
    newName = '';
  }
  function setMembers(id: string, m: string, on: boolean) {
    onsave({
      channels: channels.map((c) => (c.id !== id ? c : { ...c, members: on ? [...c.members, m] : c.members.filter((x) => x !== m) })),
    });
  }
  function removeChannel(id: string) {
    onsave({ channels: channels.filter((c) => c.id !== id) });
  }

  // 说话：旁白 / 人话 / 导演
  let text = $state('');
  let asNarrator = $state(true);
  let chan = $state('');
  let cueNote = $state('');
  async function post(path: string, body: Record<string, unknown>) {
    try {
      await api.post('/api/rooms/' + encodeURIComponent(r.id) + path, body);
      await store.refresh();
      return true;
    } catch (err) {
      store.fail(err);
      return false;
    }
  }
  async function say() {
    if (!text.trim()) return;
    if (await post('/say', { text, as: asNarrator ? '旁白' : '', channel: chan })) text = '';
  }
  async function cue(agent: string) {
    if (await post('/cue', { agent, note: cueNote, channel: chan })) cueNote = '';
  }
</script>

<h5 class="p-h">旁白和发言 <span class="muted p-small">旁白不属于任何成员；也可以以管理员的身份说</span></h5>
<div class="p-card">
  <textarea class="p-input" rows="2" bind:value={text} placeholder="例如：夜深了，庄园里的灯一盏盏熄灭……"></textarea>
  <div class="p-row">
    <label class="p-row"><input type="checkbox" bind:checked={asNarrator} /> <span>作为旁白</span></label>
    {#if channels.length}
      <select class="p-select" bind:value={chan}>
        <option value="">公开</option>
        {#each channels as c (c.id)}<option value={c.id}>频道：{c.name}</option>{/each}
      </select>
    {/if}
    <button class="btn sm primary" onclick={say}><Icon name="send" size={13} />发出</button>
  </div>
</div>

{#if r.mode === 'turns'}
  <h5 class="p-h">轮流 <span class="muted p-small">人或旁白在公开频道说一句之后，按顺序每人说一句为一轮</span></h5>
  <div class="p-card">
    <label class="p-label">这一场最多几轮（轮流必须设轮数或这一场的预算）
      <input class="p-input" inputmode="numeric" value={r.rounds ?? 0} onchange={(e) => onsave({ rounds: Number((e.target as HTMLInputElement).value) || 0 })} />
    </label>
    {#each order as m, i (m)}
      <div class="p-row">
        <span class="p-grow">{i + 1}. {name(m)}</span>
        <button class="p-lnk p-small" disabled={i === 0} onclick={() => move(i, -1)}>上移</button>
        <button class="p-lnk p-small" disabled={i === order.length - 1} onclick={() => move(i, 1)}>下移</button>
      </div>
    {/each}
    <p class="p-note p-small">已转 {r.use?.round ?? 0} 轮。到了轮数会暂停；清零累计后重新计数。重启或恢复后不会自己接着转，等下一句人话或旁白。</p>
  </div>
{/if}

{#if r.mode === 'director' && !desk}
  <h5 class="p-h">导演 <span class="muted p-small">@ 和叫名字不会触发；只有你点到的那一个会说话</span></h5>
  <div class="p-card">
    <input class="p-input" bind:value={cueNote} placeholder="给 TA 的提示（只有 TA 看得到，可空）" />
    {#each r.members as m (m)}
      <div class="p-row">
        <span class="p-grow">{name(m)}</span>
        <button class="btn sm" disabled={r.paused || r.muted.includes(m)} onclick={() => cue(m)}>让 TA 说</button>
      </div>
    {/each}
    {#if channels.length}<p class="p-note p-small">上面“旁白和发言”里选的频道，也是 TA 这一句说在哪里。</p>{/if}
  </div>
{/if}

<details class="p-more">
  <summary>私下频道（{channels.length}）</summary>
  <div class="p-card">
    {#each channels as c (c.id)}
      <div class="p-label"><b>{c.name}</b> <button class="p-lnk p-small" onclick={() => removeChannel(c.id)}>删除</button>
        {#each r.members as m (m)}
          <label class="p-row"><input type="checkbox" checked={c.members.includes(m)} onchange={(e) => setMembers(c.id, m, (e.target as HTMLInputElement).checked)} /> <span>{name(m)}</span></label>
        {/each}
      </div>
    {/each}
    {#if channels.length < 8}
      <div class="p-row">
        <input class="p-input p-grow" bind:value={newName} placeholder="新频道的名字" />
        <button class="btn sm" onclick={addChannel}>添加</button>
      </div>
    {/if}
    <p class="p-note p-small">频道里的话只有频道成员听得到；只按点名触发，轮流和主动插话只在公开频道。</p>
  </div>
</details>
