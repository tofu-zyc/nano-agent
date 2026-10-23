<script lang="ts">
 import {api,sidPath} from '../lib/api';
 import {store} from '../lib/state.svelte';
 import type {Msg} from '../lib/types';
 let {sid,summary='',busy=false,onchanged}:{sid:string;summary?:string;busy?:boolean;onchanged:()=>void}=$props();
 let turns=$state<string[]>([]);let archives=$state<{id:string;at:number;messages:number}[]>([]);let selected=$state<number[]>([]);
 async function refresh(){try{const h=await api.get<Msg[]>(sidPath(sid)+'/full-history');turns=(h??[]).filter(m=>m.role==='user').map(m=>m.content??'');archives=await api.get(sidPath(sid)+'/archives');selected=[];}catch(e){store.fail(e)}}
 async function remove(body:unknown,whole=false){if(!confirm(whole?'删除当前历史及所有归档？无法恢复。':'确定永久删除选中的历史？摘要中可能仍有概要。'))return;if(whole&&!confirm('再次确认：永久删除全部历史及归档？'))return;try{await api.post(sidPath(sid)+'/history-delete',body);await refresh();onchanged();await store.refresh();}catch(e){store.fail(e)}}
 function removeSelected(){const list=[...selected].sort((a,b)=>a-b);if(!list.length)return;if(list[list.length-1]-list[0]+1!==list.length){store.toast({kind:'err',text:'请选择一轮或连续几轮'});return}remove({from:list[0],to:list[list.length-1]})}
</script>
<details class="history" ontoggle={(e)=>{if(e.currentTarget.open)refresh()}}>
 <summary>管理历史与归档</summary>
 <div class="actions"><button class="btn sm danger" disabled={busy} onclick={()=>remove({all:true},true)}>删除全部历史</button>{#if summary}<button class="btn sm" disabled={busy} onclick={()=>remove({clear_summary:true})}>清空摘要</button>{/if}</div>
 <p>选择一轮或连续几轮删除。原始记录会永久删除；已整理的摘要可能仍有概要，可另行清空。</p>
 <div class="turns">{#each turns as text,i}<label><input type="checkbox" checked={selected.includes(i)} onchange={(e)=>selected=e.currentTarget.checked?[...selected,i]:selected.filter(x=>x!==i)} disabled={busy}/><span>{i+1}. {text.slice(-120)}</span></label>{/each}</div>
 <button class="btn sm danger" disabled={busy||!selected.length} onclick={removeSelected}>删除选中轮次</button>
 <p>归档</p>{#each archives as a}<div>{new Date(a.at*1000).toLocaleString()} · {a.messages} 条 <button class="btn sm" disabled={busy} onclick={()=>remove({archive:a.id})}>删除归档</button></div>{:else}<span>暂无归档</span>{/each}
</details>
<style>.history{padding:8px 20px;border-bottom:1px solid var(--line);font-size:12px;color:var(--muted)}.actions{display:flex;gap:8px;padding-top:8px}.turns{max-height:220px;overflow:auto}.turns label{display:flex;gap:8px;padding:5px}p{margin:8px 0}</style>
