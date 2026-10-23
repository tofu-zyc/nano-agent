<script lang="ts">
 import { onMount } from 'svelte';
 import { store } from '../lib/state.svelte';
 import { api } from '../lib/api';
 import type { ScheduleTask, ScheduleView } from '../lib/types';
 import Icon from '../components/Icon.svelte';
 let view=$state<ScheduleView>({tasks:[],timezone:''});
 let loading=$state(true);
 let busy=$state(false);
 let editing=$state(false);
 let id=$state('');
 let name=$state('');
 let when=$state('0 8 * * *');
 let prompt=$state('');
 let mode=$state('ask');
 let enabled=$state(true);
 let catchUp=$state(true);
 let timeout=$state('30m');
 let deliver=$state<string[]>([]);
 let workspaces=$state<string[]>([]);
 let error=$state('');
 const modes:Record<string,string>={readonly:'只读',ask:'审批',auto:'自动'};
 const destinations=$derived.by(()=>{
  const map=new Map<string,string>();
  for(const s of store.ov?.sessions??[]) {
   if(!s.sid.startsWith('cron:')) map.set('web:'+s.sid,'网页 · '+s.title);
   for(const ep of s.endpoints??[]) if(ep.channel!=='web') map.set(ep.key,(ep.name||ep.key)+' · '+ep.key);
  }
  for(const key of deliver) if(!map.has(key)) map.set(key,key);
  return [...map].map(([key,label])=>({key,label}));
 });
 async function load() {
  try {view=await api.get<ScheduleView>('/api/schedules');}
  catch(err){store.fail(err);}
  finally{loading=false;}
 }
 onMount(()=>{
  load();
  return store.on((e)=>{if(e.t==='schedules') load();});
 });
 function date(s:string) {
  if(!s||s.startsWith('0001'))return '无';
  try{return new Intl.DateTimeFormat('zh-CN',{timeZone:view.timezone,dateStyle:'medium',timeStyle:'short'}).format(new Date(s));}
  catch{return new Date(s).toLocaleString();}
 }
 function edit(t?:ScheduleTask) {
  id=t?.id??'';name=t?.name??'';when=t?.when??'0 8 * * *';prompt=t?.prompt??'';
  mode=t?.mode??store.session('main')?.mode??'ask';enabled=t?.enabled??true;catchUp=t?.catch_up??true;timeout=t?.timeout||'30m';
  deliver=[...(t?.deliver??['web:main'])];workspaces=[...(t?.workspaces??store.session('main')?.workspaces.map(w=>w.id)??[])];
  error='';editing=true;
 }
 function toggle(xs:string[],key:string,on:boolean){return on?[...new Set([...xs,key])]:xs.filter(x=>x!==key);}
 async function act(action:string,t:ScheduleTask,extra:Record<string,unknown>={}) {
  if(busy)return;
  if(action==='delete'&&!confirm('删除定时任务「'+t.name+'」？'))return;
  busy=true;
  try{await api.post('/api/schedules',{action,id:t.id,sid:'main',...extra});await load();}
  catch(err){store.fail(err);}
  finally{busy=false;}
 }
 async function save() {
  busy=true;error='';
  try {
   await api.post('/api/schedules',{action:id?'update':'create',id,sid:'main',name,when,prompt,mode,enabled,catch_up:catchUp,timeout,deliver,workspaces});
   editing=false;await load();store.toast({kind:'ok',text:'定时任务已保存'});
  } catch(err){error=err instanceof Error?err.message:String(err);}
  finally{busy=false;}
 }
</script>
<div class="schedules">
 <div class="heading"><div><h1>按时开始，结果送到你的窗口</h1><p>时区：{view.timezone||'加载中'} · 每次运行使用独立上下文</p></div><button class="primary" onclick={()=>edit()}><Icon name="plus" size={15}/>新建任务</button></div>
 {#if view.error}<p class="error">{view.error}</p>{/if}
 {#if loading}<p class="muted">正在加载…</p>
 {:else if !view.tasks.length}<div class="empty"><Icon name="clock" size={28}/><h2>还没有定时任务</h2><p>可以在聊天里说“每天早上 8 点给我发 AI 新闻”，也可以在这里新建。</p></div>
 {:else}
 <div class="cards">
 {#each view.tasks as t (t.id)}
 <article>
  <div class="card-head"><h2>{t.name}</h2><span class="tag" class:running={t.running}>{t.running?'运行中':t.enabled?'已开启':'已关闭'}</span></div>
  <p class="timing">{t.when} · {modes[t.mode]??t.mode}</p>
  <p class="muted">下次：{t.enabled?date(t.next):'已暂停'} · {t.catch_up?'错过一小时内补跑一次':'不补跑'}</p>
  <p class="prompt">{t.prompt}</p>
  <p class="muted targets">投递到 {t.deliver.join('、')}</p>
  {#if t.last}<p class="last" class:failed={!t.last.ok}>{t.last.ok?'已完成':'未完成'} · {date(t.last.at)}<br/>{t.last.summary.slice(0,180)}</p>{/if}
  <div class="actions">
   <button disabled={busy||t.running} onclick={()=>act('run_now',t)}><Icon name="play" size={14}/>立即运行</button>
   <button disabled={busy||t.running} onclick={()=>edit(t)}>编辑</button>
   <button disabled={busy||t.running} onclick={()=>act('update',t,{enabled:!t.enabled})}>{t.enabled?'暂停':'开启'}</button>
   <a href={'#/sessions/'+encodeURIComponent('cron:'+t.id)}>任务会话</a>
   <button class="danger" disabled={busy||t.running} onclick={()=>act('delete',t)}>删除</button>
  </div>
  {#if t.results?.length}<details><summary>最近 {t.results.length} 次结果</summary>{#each [...t.results].reverse() as r}<div class="result"><strong>{r.ok?'完成':'未完成'} · {date(r.at)}</strong><p>{r.summary}</p></div>{/each}</details>{/if}
 </article>
 {/each}
 </div>
 {/if}
 {#if editing}
 <div class="editor">
  <form onsubmit={(e)=>{e.preventDefault();save();}}>
   <div class="card-head"><h2>{id?'编辑任务':'新建任务'}</h2><button type="button" onclick={()=>editing=false} aria-label="关闭编辑"><Icon name="x" size={17}/></button></div>
   <label>名称<input bind:value={name} required placeholder="早报"/></label>
   <label>运行时间<input bind:value={when} required placeholder="0 8 * * *"/></label>
   <p class="hint">每天 8 点：0 8 * * *；每隔半小时：every 30m；一次：at 2026-10-24 08:00。使用 {view.timezone} 时区。</p>
   <label>要做什么<textarea bind:value={prompt} required rows="4" placeholder="搜索 AI 新闻，整理摘要并附上来源链接"></textarea></label>
   <div class="row"><label>权限模式<select bind:value={mode}><option value="readonly">只读</option><option value="ask">审批</option><option value="auto">自动</option></select></label><label>运行超时<input bind:value={timeout} required placeholder="30m"/></label></div>
   <p class="hint">权限不能高于创建时或当前主会话的模式。需要审批时，请在投递窗口或网页审批。</p>
   <fieldset><legend>结果投递到</legend>{#each destinations as d}<label class="check"><input type="checkbox" checked={deliver.includes(d.key)} onchange={(e)=>deliver=toggle(deliver,d.key,e.currentTarget.checked)}/>{d.label}</label>{/each}</fieldset>
   <fieldset><legend>工作目录</legend>{#each store.ov?.workspaces??[] as w}<label class="check"><input type="checkbox" checked={workspaces.includes(w.id)} onchange={(e)=>workspaces=toggle(workspaces,w.id,e.currentTarget.checked)}/>{w.host}:{w.path}</label>{/each}{#if !store.ov?.workspaces.length}<p class="hint">未添加工作目录，将在 HOME 下工作。</p>{/if}</fieldset>
   <div class="row"><label class="check"><input type="checkbox" bind:checked={enabled}/>开启任务</label><label class="check"><input type="checkbox" bind:checked={catchUp}/>错过一小时内补跑一次</label></div>
   {#if error}<p class="error" role="alert">{error}</p>{/if}
   <div class="actions"><button class="primary" disabled={busy}>{busy?'保存中…':'保存任务'}</button><button type="button" onclick={()=>editing=false}>取消</button></div>
  </form>
 </div>
 {/if}
</div>
<style>
 .schedules{flex:1;min-width:0;overflow:auto;padding:26px;position:relative}
 .heading{display:flex;align-items:center;justify-content:space-between;gap:18px;margin-bottom:24px}
 h1{font-size:21px;margin:0 0 8px}h2{font-size:16px;margin:0}.heading p,.muted,.hint{color:var(--muted);font-size:12px}
 .cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(min(100%,360px),1fr));gap:16px;align-items:start}
 article,form{border:1px solid var(--line);background:var(--panel-solid);border-radius:16px;padding:20px}
 .card-head{display:flex;align-items:center;justify-content:space-between;gap:12px}
 .tag{font-size:11px;border:1px solid var(--line);border-radius:20px;padding:3px 9px;color:var(--muted)}.tag.running{color:var(--accent)}
 .timing{font-family:var(--mono);font-size:13px}.prompt{white-space:pre-wrap;font-size:14px;line-height:1.6;max-height:110px;overflow:auto}.targets{overflow-wrap:anywhere}
 .last{padding:12px;background:var(--bg-2);border-radius:10px;font-size:12px;line-height:1.6;white-space:pre-wrap}.failed{color:var(--warn)}
 .actions{display:flex;align-items:center;gap:8px;flex-wrap:wrap}.actions a{font-size:12px;color:var(--accent);margin-left:auto}
 button{display:inline-flex;align-items:center;gap:6px;border:1px solid var(--line);border-radius:8px;padding:7px 10px;background:var(--bg-2);color:var(--text);font-size:12px}button:hover{border-color:var(--accent)}button:disabled{opacity:.45;cursor:default}
 .primary{background:var(--accent);color:white;border-color:transparent}.danger{color:var(--muted)}
 details{margin-top:16px;font-size:12px}summary{cursor:pointer;color:var(--muted)}.result{border-top:1px solid var(--line);margin-top:12px;padding-top:12px}.result p{white-space:pre-wrap;line-height:1.65;overflow-wrap:anywhere}
 .empty{padding:60px 24px;text-align:center;color:var(--muted)}.empty h2{margin:18px 0}.empty p{font-size:13px}
 .editor{position:fixed;inset:58px 0 0 72px;background:color-mix(in srgb,var(--bg) 70%,transparent);backdrop-filter:blur(5px);z-index:9;overflow:auto;padding:24px;display:grid;align-items:start;justify-items:center}
 form{width:min(680px,100%);display:flex;flex-direction:column;gap:16px}
 label{display:flex;flex-direction:column;gap:7px;font-size:13px}input,textarea,select{background:var(--bg-2);border:1px solid var(--line);color:var(--text);border-radius:8px;padding:9px 10px;font:inherit;width:100%}textarea{resize:vertical}.hint{margin:0;line-height:1.6}.row{display:flex;gap:20px}.row>label{flex:1}.check{flex-direction:row;align-items:center;margin:6px 0;font-size:12px}.check input{width:auto}
 fieldset{border:1px solid var(--line);border-radius:10px;padding:10px 14px;max-height:190px;overflow:auto}legend{font-size:12px;color:var(--muted)}.error{color:var(--warn);font-size:13px}
 @media(max-width:700px){.schedules{padding:16px}.heading{align-items:start}h1{font-size:17px}.heading>button{white-space:nowrap}.editor{padding:12px}form{padding:16px}}
</style>
