<script lang="ts">
  import { onMount } from 'svelte';
  import { api } from '../lib/api';
  import { store } from '../lib/state.svelte';
  import Icon from '../components/Icon.svelte';

  type Connection = { id:string; name:string; host:string; port:number; user:string; identity_file:string; has_password:boolean; auto_connect:boolean; state:string; detail?:string };
  type PairRequest = { id:string; node:string; addr:string; code:string; at:string };
  type Paired = { node:string; addr:string; since:string; online:boolean };
  type Devices = { connections:Connection[]; ssh_hosts:string[]; listener:string; primary_listener:string; arch:string; pairing:PairRequest[]; paired:Paired[] };
  const labels:Record<string,string> = {online:'已连接',offline:'未连接',connecting:'连接中',reconnecting:'等待重连',disconnecting:'断开中',error:'连接失败'};
  let data = $state<Devices|null>(null);
  let error = $state('');
  let busy = $state('');
  let editor = $state(false);
  let gateway = $state(location.hostname+':7700');
  let listen = $state('0.0.0.0:7700');
  let form = $state(empty());
  const activeListener = $derived(data?.listener || (data?.primary_listener?.startsWith('tcp:') ? data.primary_listener.slice(4) : ''));
  const primaryTCP = $derived(!!data?.primary_listener?.startsWith('tcp:'));
  const online = $derived(store.ov?.kernels ?? []);
  let refreshing = false;

  function empty() { return {id:'',name:'',host:'',port:undefined as number|undefined,user:'',identity_file:'',password:'',has_password:false,clear_password:false,auto_connect:false}; }
  async function refresh() {
    if (refreshing) return;
    refreshing=true;
    try {
      const next=await api.get<Devices>('/api/devices');
      if (!data && next.listener) { listen=next.listener; gateway=location.hostname+':'+next.listener.split(':').pop(); }
      data=next;error='';
    } catch(e) { error=e instanceof Error?e.message:String(e); }
    finally { refreshing=false; }
  }
  onMount(() => { refresh();const timer=setInterval(refresh,2000);return()=>clearInterval(timer); });
  function edit(c?:Connection) { form=c?{...empty(),...c,port:c.port||undefined}:empty();editor=true; }
  async function save(connect:boolean) {
    busy='save';
    try {
      const result=await api.post<{id:string}>('/api/devices',{...form,port:form.port??0});
      form=empty();editor=false;
      if(connect) await api.post(`/api/devices/${result.id}/connect`);
      await refresh();
    } catch(e) { store.fail(e); } finally {busy='';}
  }
  async function action(c:Connection,op:'connect'|'disconnect'|'delete') {
    busy=c.id;
    try { if(op==='delete') await api.del(`/api/devices/${c.id}`); else await api.post(`/api/devices/${c.id}/${op}`);await refresh(); }
    catch(e) {store.fail(e);} finally {busy='';}
  }
  async function listener(enable:boolean) {
    busy='listener';
    try { await api.put('/api/devices/listener',{address:enable?listen:''}); if(enable) gateway=location.hostname+':'+listen.split(':').pop(); await refresh();store.toast({kind:'ok',text:enable?'电脑接入已启用':'电脑接入已关闭'}); }
    catch(e) {store.fail(e);} finally {busy='';}
  }
  async function decide(r:PairRequest, ok:boolean) {
    busy=r.id;
    try { await api.post(`/api/pairing/${r.id}`,{ok}); store.toast({kind:'ok',text:ok?`${r.node} 已接入，稍等它重新连上来`:`已拒绝 ${r.node}`}); await refresh(); }
    catch(e) {store.fail(e);} finally {busy='';}
  }
  async function unpair(d:Paired) {
    if (!confirm(`移除 ${d.node}？它的口令会作废${d.online?'，现在就会断开':''}。要再接入，需要重新配对。`)) return;
    busy=d.node;
    try { await api.del(`/api/paired/${encodeURIComponent(d.node)}`); await refresh(); }
    catch(e) {store.fail(e);} finally {busy='';}
  }
  const day = (s:string) => new Date(s).toLocaleDateString();
  function download(platform:string) { location.assign('/api/devices/package?platform='+platform+'&gateway='+encodeURIComponent(gateway)); }
</script>

<div class="devices">
  <div class="heading"><div><span class="eyebrow">把工作带到需要的机器上</span><h1>连接电脑与服务器</h1><p>电脑运行启动器连进来；服务器由网关通过 SSH 连接。上线后，在会话里选择工作机器和目录。</p></div><span class="online-count"><i class="dot online"></i>{online.length} 台在线</span></div>
  {#if error}<div class="error-banner">{error}</div>{/if}
  {#each data?.pairing??[] as r (r.id)}
    <section class="pair-request">
      <span class="method-icon"><Icon name="link" size={22}/></span>
      <div class="pair-who"><span class="eyebrow">新电脑请求接入</span><h2>{r.node}</h2><p>来自 {r.addr}。核对这台电脑上显示的确认码，一致再允许。</p></div>
      <div class="pair-code" title="确认码">{r.code.slice(0,3)} {r.code.slice(3)}</div>
      <div class="pair-acts"><button disabled={busy===r.id} onclick={()=>decide(r,false)}>拒绝</button><button class="primary" disabled={busy===r.id} onclick={()=>decide(r,true)}><Icon name="check" size={14}/>允许接入</button></div>
    </section>
  {/each}
  <div class="methods">
    <section class="method computer">
      <span class="method-icon"><Icon name="terminal" size={23}/></span>
      <div><span class="eyebrow">在另一台电脑启动</span><h2>电脑启动包</h2><p>解压后启动，填写网关地址和电脑名字。第一次连接时这里会出现请求，核对确认码后点允许，不用抄口令。</p></div>
      <label>这台网关的 IP 或域名<input bind:value={gateway} placeholder="192.168.1.2:7700" aria-label="网关接入地址"/></label>
      {#if !activeListener}<button disabled={!!busy} onclick={()=>listener(true)}>先启用电脑接入 · 7700 端口</button>{/if}
      <div class="downloads"><button class="primary" disabled={!activeListener||!gateway} onclick={()=>download('windows')}><Icon name="down"/>Windows · WSL2</button><button disabled={!activeListener||!gateway} onclick={()=>download('linux')}><Icon name="down"/>Linux</button></div>
      <p class="hint">Windows 双击 start.cmd，选择 WSL2 环境；Linux 解压后运行 sh start.sh。首次跨发行版启动可能需要 C 编译器和 make。</p>
      <details><summary><span class="dot {activeListener?'online':'idle'}"></span>{activeListener?'接入已启用 · '+activeListener:'设置电脑接入地址'}</summary><div class="listen"><input aria-label="监听地址" bind:value={listen} placeholder="0.0.0.0:7700"/><button disabled={!!busy} onclick={()=>listener(true)}>启用</button><button disabled={!!busy||!data?.listener||primaryTCP} onclick={()=>listener(false)}>关闭</button></div><p class="hint">使用网关的局域网或私有网络地址。这个端口用于电脑接入；SSH 服务器不需要它。</p></details>
    </section>
    <section class="method ssh">
      <span class="method-icon"><Icon name="globe" size={23}/></span>
      <div><span class="eyebrow">由网关主动连接</span><h2>SSH 服务器</h2><p>填写主机、端口和用户，使用密码或密钥。也可以直接选择网关已有的 SSH 别名。</p></div>
      <div class="steps"><span><b>1</b>建立 SSH 连接</span><span><b>2</b>自动准备内核</span><span><b>3</b>在会话中开始工作</span></div>
      <button class="primary add" onclick={()=>edit()}><Icon name="plus"/>添加服务器</button>
      <p class="hint">内核通信走 SSH 加密通道，无需在服务器开放额外端口。服务器不能运行包内程序时会自动从源码构建。</p>
    </section>
  </div>

  {#if editor}
    <section class="editor">
      <div class="section-title"><h2>{form.id?'编辑服务器':'添加服务器'}</h2><button aria-label="关闭编辑" onclick={()=>{editor=false;form=empty();}}><Icon name="x"/></button></div>
      <form onsubmit={(e)=>{e.preventDefault();save(true);}}>
        <div class="fields">
          <label>主机地址或 SSH 别名<input required bind:value={form.host} list="ssh-hosts" placeholder="192.168.1.10 或 my-server"/></label>
          <label>在会话中显示的名字<input required bind:value={form.name} placeholder="开发服务器"/></label>
          <label>SSH 用户<input bind:value={form.user} placeholder="留空沿用 SSH 配置" autocomplete="off"/></label>
          <label>端口<input type="number" min="1" max="65535" bind:value={form.port} placeholder="留空沿用 SSH 配置，通常为 22"/></label>
          <label>密码<input type="password" bind:value={form.password} placeholder={form.has_password?'已保存，留空保持':'用密钥或 SSH 配置连接时可留空'} autocomplete="new-password"/></label>
          <label>密钥路径<input bind:value={form.identity_file} placeholder="可选，例如 ~/.ssh/id_ed25519"/></label>
        </div>
        <datalist id="ssh-hosts">{#each data?.ssh_hosts??[] as host}<option value={host}></option>{/each}</datalist>
        <div class="options"><label><input type="checkbox" bind:checked={form.auto_connect}/>网关启动时连接，掉线后重连</label>{#if form.has_password}<label><input type="checkbox" bind:checked={form.clear_password}/>清除已保存的密码</label>{/if}</div>
        <div class="form-actions"><span class="hint">SSH 配置中的密钥、跳板机和代理仍会生效。</span><button type="button" disabled={!!busy} onclick={()=>save(false)}>仅保存</button><button class="primary" type="submit" disabled={!!busy}>{busy==='save'?'正在保存…':'保存并连接'}</button></div>
      </form>
    </section>
  {/if}

  <div class="section-title"><h2>服务器连接 <span>{data?.connections.length??0}</span></h2><button onclick={refresh}><Icon name="refresh"/>刷新</button></div>
  <div class="connections">
    {#each data?.connections??[] as c (c.id)}
      {@const active=['online','connecting','reconnecting','disconnecting'].includes(c.state)}
      <article class="connection" class:connected={c.state==='online'}>
        <div class="connection-head"><span class="machine-icon"><Icon name="terminal" size={21}/></span><div class="identity"><h3>{c.name}</h3><code>{c.user?c.user+'@':''}{c.host}{c.port?':'+c.port:''}</code></div><span class="status"><i class="dot {c.state==='online'?'online':c.state==='error'?'error':active?'working':'idle'}"></i>{labels[c.state]??c.state}</span></div>
        {#if c.detail}<pre class="detail">{c.detail}</pre>{:else}<p class="hint">{c.has_password?'密码认证':'SSH 配置 / 密钥认证'}{c.auto_connect?' · 自动连接':''}</p>{/if}
        <div class="connection-actions">
          {#if c.state==='online'}<button onclick={()=>store.open({type:'kernel',node:c.name})}>查看机器<Icon name="right" size={13}/></button>{/if}
          <div class="grow"></div>
          {#if active}<button disabled={busy===c.id||c.state==='disconnecting'} onclick={()=>action(c,'disconnect')}>断开连接</button>
          {:else}<button disabled={!!busy} onclick={()=>edit(c)}>编辑</button><button class="icon-button" aria-label="删除 {c.name}" disabled={!!busy} onclick={()=>action(c,'delete')}><Icon name="trash" size={15}/></button><button class="primary" disabled={!!busy} onclick={()=>action(c,'connect')}>连接</button>{/if}
        </div>
      </article>
    {:else}<div class="empty"><Icon name="link" size={30}/><h3>把第一台服务器接进来</h3><p>已有 SSH 别名可以直接使用，无需重新填写全部配置。</p><button onclick={()=>edit()}>添加服务器<Icon name="plus" size={14}/></button></div>{/each}
  </div>
  {#if data?.paired.length}
    <div class="section-title"><h2>配对的电脑 <span>{data.paired.length}</span></h2></div>
    <div class="paired-list">
      {#each data.paired as d (d.node)}
        <div class="paired"><span class="dot {d.online?'online':'idle'}"></span><b>{d.node}</b><span class="hint">{d.online?'在线':'离线'} · {d.addr} · {day(d.since)} 配对</span><div class="grow"></div>{#if d.online}<button onclick={()=>store.open({type:'kernel',node:d.node})}>查看机器</button>{/if}<button class="icon-button" aria-label="移除 {d.node}" title="移除：口令作废，要再接入需重新配对" disabled={busy===d.node} onclick={()=>unpair(d)}><Icon name="trash" size={14}/></button></div>
      {/each}
    </div>
  {/if}
  {#if online.length}<div class="section-title"><h2>在线内核</h2></div><div class="online-list">{#each online as n}<button onclick={()=>store.open({type:'kernel',node:n.node})}><span class="dot online"></span>{n.node}<span class="hint">{n.local?'本机':'远程'}</span><Icon name="right" size={13}/></button>{/each}</div>{/if}
</div>

<style>
  .devices{flex:1;overflow:auto;padding:30px clamp(20px,4vw,64px) 50px;min-width:0}.heading{display:flex;align-items:center;justify-content:space-between;gap:20px;margin-bottom:26px}.eyebrow{font-size:11px;color:var(--accent-2);letter-spacing:.06em}h1{font-size:27px;letter-spacing:-.03em;margin:7px 0 10px}h2{font-size:17px;margin:5px 0 8px}h3{font-size:15px;margin:0 0 4px}p{color:var(--muted);font-size:13px;line-height:1.7;margin:0}.online-count{display:flex;align-items:center;gap:8px;white-space:nowrap;font-size:12px;color:var(--text-2)}.methods{display:grid;grid-template-columns:1fr 1fr;gap:20px;margin-bottom:30px}.method,.editor,.connection{border:1px solid var(--line);background:var(--panel);border-radius:17px}.method{padding:24px;display:flex;flex-direction:column;gap:17px;position:relative;overflow:hidden}.method:before{content:'';position:absolute;inset:0;background:radial-gradient(ellipse at 95% 0,color-mix(in srgb,var(--accent) 9%,transparent),transparent 70%);pointer-events:none}.method-icon,.machine-icon{display:grid;place-items:center;color:var(--c-kernel);background:color-mix(in srgb,var(--c-kernel) 12%,transparent);border:1px solid color-mix(in srgb,var(--c-kernel) 16%,transparent);width:44px;height:44px;border-radius:13px}.ssh .method-icon{color:var(--accent-2);background:color-mix(in srgb,var(--accent) 12%,transparent)}label{display:flex;flex-direction:column;gap:7px;font-size:12px;color:var(--text-2)}input:not([type=checkbox]){width:100%;height:38px;border:1px solid var(--line-2);background:var(--bg-2);color:var(--text);border-radius:8px;padding:0 11px;outline:none;font:inherit;box-sizing:border-box}input:focus{border-color:var(--accent)}button{display:inline-flex;align-items:center;justify-content:center;gap:7px;border:1px solid var(--line-2);background:var(--panel-2);color:var(--text);border-radius:8px;padding:8px 13px;font-size:12px;cursor:pointer}button:hover{border-color:var(--accent)}button:disabled{opacity:.5;cursor:default}.primary{background:var(--accent);border-color:var(--accent);color:#fff}.downloads{display:flex;gap:10px}.downloads button{flex:1}.hint{font-size:11.5px;color:var(--muted);line-height:1.7}.steps{display:flex;flex-direction:column;gap:13px;margin:5px 0}.steps span{display:flex;align-items:center;gap:10px;color:var(--text-2);font-size:12px}.steps b{width:22px;height:22px;display:grid;place-items:center;background:var(--panel-2);border:1px solid var(--line);border-radius:7px;color:var(--accent-2);font-size:10px}.add{align-self:flex-start}.ssh .hint{margin-top:auto}details{border-top:1px solid var(--line);padding-top:13px}summary{cursor:pointer;font-size:11.5px;color:var(--text-2);display:flex;gap:8px;align-items:center}.listen{display:flex;gap:7px;margin:13px 0 8px}.listen input{min-width:0;flex:1;width:0}.listen button{white-space:nowrap;flex:none}.editor{padding:22px;margin-bottom:28px;background:var(--panel-solid)}.section-title{display:flex;align-items:center;justify-content:space-between;margin:0 0 14px;gap:10px}.section-title h2 span{font-size:12px;color:var(--muted);margin-left:7px}.section-title button{background:transparent;border-color:var(--line)}.fields{display:grid;grid-template-columns:1fr 1fr;gap:17px}.options{display:flex;gap:20px;margin:20px 0}.options label{display:flex;flex-direction:row;align-items:center;gap:7px}.form-actions{display:flex;gap:8px;align-items:center}.form-actions .hint{margin-right:auto}.connections{display:grid;grid-template-columns:repeat(auto-fill,minmax(330px,1fr));gap:16px;margin-bottom:28px}.connection{padding:19px;min-width:0}.connection.connected{border-color:color-mix(in srgb,var(--c-kernel) 40%,var(--line))}.connection-head{display:flex;align-items:center;gap:12px;margin-bottom:16px}.identity{min-width:0;flex:1}.identity code{font-size:11px;color:var(--muted);overflow-wrap:anywhere}.status{display:flex;gap:6px;align-items:center;font-size:10.5px;color:var(--text-2);white-space:nowrap}.detail{font:11px/1.6 var(--mono,monospace);white-space:pre-wrap;overflow-wrap:anywhere;max-height:130px;overflow:auto;color:var(--muted);background:var(--bg-2);padding:10px;border-radius:8px}.connection-actions{display:flex;gap:7px;align-items:center;border-top:1px solid var(--line);padding-top:13px;margin-top:14px}.grow{flex:1}.icon-button{padding:8px}.empty{grid-column:1/-1;border:1px dashed var(--line-2);border-radius:14px;text-align:center;padding:30px;color:var(--muted)}.empty h3{margin-top:12px;color:var(--text-2)}.empty button{margin-top:15px}.online-list{display:flex;flex-wrap:wrap;gap:10px}.online-list button{padding:11px 14px}.online-list .hint{margin:0 12px 0 2px}.error-banner{border:1px solid var(--warn);padding:12px;border-radius:9px;margin-bottom:15px;color:var(--warn)}.dot.working{background:var(--warn)}.pair-request{display:flex;align-items:center;gap:18px;padding:18px 22px;margin-bottom:18px;border:1px solid color-mix(in srgb,var(--accent) 55%,var(--line));background:linear-gradient(100deg,color-mix(in srgb,var(--accent) 12%,var(--panel)),var(--panel));border-radius:17px;animation:rise .25s ease}.pair-who{flex:1;min-width:0}.pair-who h2{margin:3px 0 4px}.pair-code{font:600 26px/1 var(--mono,monospace);letter-spacing:.12em;color:var(--text);padding:10px 16px;border-radius:11px;background:var(--bg-2);border:1px solid var(--line-2);white-space:nowrap}.pair-acts{display:flex;gap:8px}.paired-list{display:flex;flex-direction:column;gap:8px;margin-bottom:28px}.paired{display:flex;align-items:center;gap:10px;padding:11px 14px;border:1px solid var(--line);background:var(--panel);border-radius:12px;min-width:0}.paired b{font-size:13px}.paired .hint{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;min-width:0}.dot.idle{background:var(--muted)}@media(max-width:850px){.pair-request{flex-wrap:wrap}.pair-acts{width:100%;justify-content:flex-end}.methods{grid-template-columns:1fr}.heading{align-items:flex-start}.devices{padding:22px}.fields{grid-template-columns:1fr}.form-actions{flex-wrap:wrap}.form-actions .hint{width:100%;margin-bottom:5px}.options{flex-direction:column;gap:10px}}
</style>
