"use strict";
let advancedDirty=false, advancedRevision=0, policyDirty=false, policyRevision=0, poolRows=[];
const advancedFields={request_limit_mib:"request-limit",zstd_window_mib:"window-limit",compact_limit_mib:"compact-limit"};
let nextPage="configuration",nextFocus="";
function renderUpgrade(s){
 if(typeof renderProbeChain === "function") renderProbeChain(s);
 let title="已接上，可以开始使用",help="重启 Codex、新建对话，发一句话即可。无需反复接入。";nextPage="overview";nextFocus="";
 if(s.config_error){title="连接配置还差一步";help=s.config_error;nextPage="configuration";if(s.config_error.includes("base_url")){help="中转地址没填完整。点右边按钮，填写服务商提供的地址，再点“只补全缺失的 base_url”。";nextFocus="relay-base";}}
 else if(s.route_error || !s.routes){title="先加一个可用连接";help="已有代理软件就填本地地址；有多个订阅链接，可以选 TXT 一起导入。";nextPage="sources";}
 else if(!s.configured_codex && !s.configuration_writable){title="当前是只读启动，尚未自动接入";help="请使用正常启动入口接入 Codex；本次启动不会改你的配置。可先点下方一键检测环境配置。";nextPage="overview";nextFocus="environment-check";}
 else if(!s.configured_codex){title="服务已启动，还没接上 Codex";help="点“一键接入 Codex”；看到已接管后，重启 Codex 并新建对话。";nextPage="overview";nextFocus="quick-setup";}
 else if(s.sessions?.some(x=>x.phase==="rate_limited"||x.phase==="auth_blocked")){title="上游要求先暂停";help="限流请等待；登录/权限错误请回 Codex 或服务商处理。换节点和反复重试不会解除它。";}
 else if(s.sessions?.some(x=>x.phase==="waiting_for_state")){title="还没拿到符合规则的值";help="先检查代理连接，再到代理池选择一个节点点“只试这个节点”。冷却期间等倒计时结束，不要连点。";nextPage="pool";}
 $("next-step-title").textContent=title;$("next-step-help").textContent=help;$("next-step-action").textContent=nextPage==="overview"&&!nextFocus?"查看会话":"带我去处理";

 if(!advancedDirty && s.advanced){
  for(const [key,id] of Object.entries(advancedFields)) $(id).value=s.advanced[key];
  $("egress-mode").value=s.advanced.egress_mode||"state";
  $("pool-enabled").checked=s.advanced.pool_enabled;
  $("external-only").checked=s.advanced.external_proxy_only;
  const id=s.advanced.egress_route;
  if(id && ![...$("egress-route").options].some(o=>o.value===id)) $("egress-route").add(new Option(id,id));
  $("egress-route").value=id||"";
 }
 if(!policyDirty && document.activeElement!==$("state-policy")) $("state-policy").value=s.state_refresh_mode||"standby";
 const selected=$("pool-session").value;$("pool-session").replaceChildren(new Option("请选择现有模型会话",""));
 for(const session of s.sessions||[]) $("pool-session").add(new Option(`${session.model} · ${session.expected_length||"规则待识别"} · ${phases[session.phase]||session.phase}`,session.id));
 if([...$("pool-session").options].some(o=>o.value===selected))$("pool-session").value=selected;
 else if(s.sessions?.length===1)$("pool-session").value=s.sessions[0].id;
}
$("state-policy").addEventListener("change",()=>{policyDirty=true;policyRevision++;});
$("save-state-policy").addEventListener("click",()=>action(async()=>{
 const revision=policyRevision;const r=await api("state-policy",{mode:$("state-policy").value});if(revision===policyRevision)policyDirty=false;$("state-policy-result").textContent=r.message;await refresh();
}));
$("advanced-form").addEventListener("input",()=>{advancedDirty=true;advancedRevision++;});
$("pool-preset").addEventListener("click",()=>{advancedDirty=true;advancedRevision++;$("egress-mode").value="random";$("pool-enabled").checked=true;notice("已填入；保存后才生效。独立随机至少需要两个不同节点，耗尽后请手动回收。");});
$("advanced-form").addEventListener("submit",event=>{event.preventDefault();action(async()=>{
 if(!confirm("保存会清空 state 缓存，但保留节点已用/失败记录。更大的请求限制会占更多内存；跨出口使用 state 可能被上游拒绝。继续？"))return;
 const body=Object.fromEntries(Object.entries(advancedFields).map(([key,id])=>[key,Number($(id).value)]));
 Object.assign(body,{egress_mode:$("egress-mode").value,egress_route:$("egress-route").value,pool_enabled:$("pool-enabled").checked,external_proxy_only:$("external-only").checked});
 const revision=advancedRevision;const r=await api("advanced",body);if(revision===advancedRevision)advancedDirty=false;$("advanced-result").textContent=r.message;await refresh();
});});
async function loadPool(){
 const data=await api("pool",{});poolRows=data.routes||[];
 const selected=$("egress-route").value;$("egress-route").replaceChildren(new Option("请选择节点",""));
 for(const row of poolRows)$("egress-route").add(new Option(`${row.label} · ${row.id}`,row.id));$("egress-route").value=selected;
 $("pool-summary").textContent=`共 ${poolRows.length} 个节点；用后移出${data.enabled?"已开启":"未开启（兼容循环模式）"}。不会把节点数量当成独立公网 IP 数量。`;
 renderPool();
}
const poolReasons={manual:"手动调整",accepted:"已取得符合规则的值",probe_started:"已开始探测",request_started:"已派发请求",request_dispatched:"已使用",request_failed:"请求未成功",shape_mismatch:"返回值不符合规则",state_time_rejected:"已过期或时间异常",missing_state_header:"未返回 state",invalid_state_envelope:"state 格式无法识别",model_capacity:"上游模型繁忙",upstream_rate_limited:"上游要求暂停",response_failed:"上游报告失败",incomplete_response:"回复未完整结束",network_failed:"连接失败或超时",upstream_rejected:"上游拒绝请求"};
function renderPool(){
 $("pool-list").replaceChildren();const rows=poolRows.filter(r=>r.state===$("pool-filter").value);
 for(const row of rows){
  const box=textNode("div","","card");const label=textNode("label","");const checkbox=document.createElement("input");checkbox.type="checkbox";checkbox.dataset.poolId=row.id;
  label.append(checkbox,document.createTextNode(`${row.label||row.id} · ${row.protocol}`));box.append(label,textNode("p",`${row.id} · 尝试 ${row.attempts} 次 · ${poolReasons[row.reason]||(row.reason?"未成功，请查看会话提示":"尚未使用")}`,"hint"));
  const retry=textNode("button","只试这个节点","secondary");retry.disabled=row.state==="disabled";retry.dataset.locked=String(retry.disabled);
  retry.addEventListener("click",()=>action(async()=>{
   const id=$("pool-session").value;if(!id){notice("先在 Codex 发一条消息并刷新，然后选择会话。不会自动读取账号文件。");return;}
   if(!confirm("使用所选会话的凭据，在此节点发送一次短模型探测，可能消耗额度。仍遵守冷却和上游停止规则。继续？"))return;
   try{const r=await api("state/retry",{id,route_id:row.id});$("pool-result").textContent=r.message;}
   finally{await refresh();await loadPool();}
  }));box.append(retry);$("pool-list").append(box);
 }
 if(!rows.length)$("pool-list").append(textNode("p","当前清单为空。","hint"));
}
$("pool-filter").addEventListener("change",renderPool);
for(const id of ["reload-pool","load-egress"])$(id).addEventListener("click",()=>action(async()=>{await refresh();await loadPool();}));
$("pool-select-all").addEventListener("click",()=>{const boxes=[...document.querySelectorAll('[data-pool-id]')];const checked=!boxes.every(b=>b.checked);boxes.forEach(b=>b.checked=checked);});
for(const [id,target] of [["pool-recycle","available"],["pool-disable","disabled"]])$(id).addEventListener("click",()=>action(async()=>{
 const ids=[...document.querySelectorAll('[data-pool-id]:checked')].map(b=>b.dataset.poolId);if(!ids.length){notice("先选择节点。");return;}
 if(!confirm(`处理选中的 ${ids.length} 个节点？不会修改订阅文件或跳过冷却。`))return;
 const r=await api("pool/change",{ids,state:target});$("pool-result").textContent=r.message;await loadPool();
}));
$("subscription-txt").addEventListener("change",async()=>{
 const file=$("subscription-txt").files[0];if(!file)return;if(file.size>65536){notice("TXT 不能超过 64 KiB。");return;}
 try{$("subscription-lines").value=await file.text();}catch{notice("文件读取失败，请使用 UTF-8 TXT。");}
});
function importBody(){return {mode:"subscription-list",value:$("subscription-lines").value,append:$("subscription-append").checked,enable_pool:$("subscription-enable-pool").checked};}
$("test-pool-import").addEventListener("click",()=>action(async()=>{const r=await api("sources/test",importBody());$("pool-import-result").textContent=`${r.message} 合并后 ${r.routes.length} 个节点。`;}));
$("pool-import-form").addEventListener("submit",event=>{event.preventDefault();action(async()=>{
 if(!confirm(`${$("subscription-append").checked?"追加":"替换"}订阅并应用？旧配置会备份，旧 state 清空，已用/失败节点记录保留。仅使用你有权使用的来源。`))return;
 const r=await api("sources/apply",importBody());$("pool-import-result").textContent=r.message;$("subscription-lines").value="";$("subscription-txt").value="";await refresh();
});});
$("relay-models-form").addEventListener("submit",event=>{event.preventDefault();action(async()=>{
 try{const r=await api("relay-models",{base_url:$("relay-base").value,api_key:$("relay-key").value});$("relay-result").textContent=`${r.message}\n支持模型：${r.models?.join("、")||"未列出支持模型"}`;}
 finally{$("relay-key").value="";}
});});
$("repair-relay").addEventListener("click",()=>action(async()=>{
 if(!$("relay-base").value){notice("请填写中转基础地址。");return;}
 const preview=await api("codex-config/preview",{});
 if(!confirm("仅补全当前中转缺失的 base_url，先备份并保留其他设置；已有地址不会覆盖。Key 必须先在 Codex/CCS 配好。继续？"))return;
 const r=await api("relay-repair",{upstream:$("relay-base").value,expected_config_sha256:preview.config_sha256,expected_exists:preview.exists});$("relay-result").textContent=r.message;await refresh();
}));

$("next-step-action").addEventListener("click",()=>{
 document.querySelector(`button[data-page="${nextPage}"]`).click();
 if(nextFocus){$(nextFocus).scrollIntoView({block:"center",behavior:"smooth"});$(nextFocus).focus();}
});
