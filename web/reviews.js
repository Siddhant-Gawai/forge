'use strict';
const diffCache=new Map();
function remoteCall(action,m,extra={}){return api('reviews',{action,project_id:m?.project_id||currentProject,id:m?.id||'',head_sha:m?.remote?.head_sha||'',...extra})}
function currentReviews(m){const latest=new Map();for(const r of m.remote.reviews||[])latest.set(r.provider_user_id,r);return [...latest.values()]}
function reviewBlocks(m){
 const p=project(),v=m.remote,blocks=[];
 const eligible=r=>r.provider_user_id!==v.author_id&&Math.max(ranks[p.members[r.user_id]]||0,ranks[org()?.members[r.user_id]]||0)>=2;
 const reviews=currentReviews(m).filter(eligible),approvals=reviews.filter(r=>r.decision==='approve'&&r.head_sha===v.head_sha).length;
 if(v.needs_sync)blocks.push('Provider activity received. Refresh before reviewing.');
 if(v.draft)blocks.push('Mark the request ready for review at the provider.');
 if(!v.mergeable)blocks.push(`Provider merge status: ${v.merge_status||'checking'}`);
 if(p.require_pipeline&&v.ci!=='passed')blocks.push(`Provider CI: ${v.ci}. A passing pipeline is required.`);
 if(approvals<p.required_approvals)blocks.push(`${approvals} of ${p.required_approvals} required approvals on this commit.`);
 if(reviews.some(r=>r.decision==='request_changes'))blocks.push('A reviewer has requested changes. Their approval is required to clear it.');
 if(m.comments.some(c=>!c.resolved))blocks.push('Resolve all review discussions.');
 if(v.merge_attempt)blocks.push('A merge was submitted. Refresh to reconcile its outcome; check the provider if still uncertain.');
 return {blocks,approvals};
}
function remoteDetail(m){
 const v=m.remote,{blocks,approvals}=reviewBlocks(m),p=project(),key=m.id+':'+v.head_sha+':'+v.base_sha,files=diffCache.get(key);
 const connect=`<a class="button" href="/api/oauth/${esc(p.repository.provider)}/start?project_id=${encodeURIComponent(p.id)}&review=1">Connect my ${esc(p.repository.provider)} account ↗</a>`;
 return heading(m.title,`${m.source} → ${m.target}`,button('← All requests','back'))+
 `<div class="review-summary">${badge(m.state)} <strong>${esc(p.repository.provider)} #${v.number}</strong><span>by ${esc(v.author)}</span><code>${esc(v.head_sha.slice(0,12))}</code><a href="${esc(v.url)}" target="_blank" rel="noopener noreferrer">Open at provider ↗</a></div>`+
 card('Merge readiness',`<div class="repo-details"><p><strong>${approvals} / ${p.required_approvals} approvals</strong>${badge(v.ci)}</p>${blocks.length?`<ul class="merge-blockers">${blocks.map(b=>`<li>${esc(b)}</li>`).join('')}</ul>`:'<p class="ready-message">Review requirements satisfied. Provider status will be checked again when you merge.</p>'}<p>Last synced ${age(v.synced_at)} · ${esc(v.merge_status)}</p><div class="detail-actions">${button('Refresh provider status','remote-sync','button',m.id)}${connect}${m.state==='open'&&level()>=3?button('Merge into '+esc(m.target),'remote-merge','primary',m.id):''}</div></div>`)+
 card('Description',`<div class="detail-body">${esc(m.body||'No description.')}</div>`)+
 card('Changed files',`<div class="detail-actions">${button(files?'Reload diff':'Load diff','remote-diff','button',m.id)}</div>${files?files.map((f,i)=>renderDiff(m,f,i)).join('')||empty('No changed files returned by the provider.'):empty('Load the current commit’s diff to inspect changes and comment on a line.')}<div class="note">Binary, oversized, or provider-omitted patches must be inspected at the provider.</div>`)+
 card('Forge reviews',`<div class="note">Reviews and discussions are stored in Forge. Provider approval rules also apply. New commits invalidate approvals; requests for changes stay blocking until that reviewer approves.</div>${m.state==='open'&&level()>=2?`<div class="detail-actions">${button('Approve','remote-approve','button',m.id)}${button('Request changes','remote-changes','button',m.id)}${button('General comment','remote-comment','button',m.id)}</div>`:''}${[...(v.reviews||[])].reverse().map(r=>`<div class="row"><div class="row-main"><div class="row-title">${esc(userName(r.user_id))} · ${r.decision==='approve'?'Approved':'Requested changes'} ${r.head_sha!==v.head_sha?'<span class="badge">Older commit</span>':''}</div><div class="row-meta">${esc(r.head_sha.slice(0,12))} · ${age(r.created_at)}</div><div class="detail-body">${esc(r.body)}</div></div></div>`).join('')||empty('No reviews yet.')}`)+
 card('Discussions',m.comments.map(c=>`<div class="row"><div class="row-main"><div class="row-title">${esc(userName(c.author_id))} ${c.resolved?badge('closed'):badge('open')} ${c.commit_sha!==v.head_sha?'<span class="badge">Older commit</span>':''}</div><div class="row-meta">${esc(c.path?c.path+':'+c.line+' ('+c.side+')':'General comment')} · ${esc((c.commit_sha||'').slice(0,12))}</div><div class="detail-body">${esc(c.body)}</div></div>${!c.resolved&&m.state==='open'?`<button class="button" data-action="resolve" data-id="${esc(m.id)}" data-comment="${esc(c.id)}">Resolve</button>`:''}</div>`).join('')||empty('No discussions yet.'));
}
function renderDiff(m,f,index){
 if(f.unavailable)return `<details class="diff-file"><summary>${esc(f.path)} · Preview unavailable</summary><p class="note">Inspect this file at the provider.</p></details>`;
 let old=0,line=0,inHunk=false;
 const rows=f.patch.split('\n').map(text=>{
  const h=text.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/);
  if(h){old=Number(h[1]);line=Number(h[2]);inHunk=true;return `<div class="diff-hunk">${esc(text)}</div>`}
  let left='',right='',side='',number=0,cls='';
  if(inHunk){if(text.startsWith('+')){right=line++;side='new';number=right;cls='added'}else if(text.startsWith('-')){left=old++;side='old';number=left;cls='removed'}else if(text.startsWith(' ')){left=old++;right=line++;side='new';number=right}}
  const action=side&&m.state==='open'&&level()>=2?`data-action="remote-inline" data-id="${esc(m.id)}" data-file="${index}" data-line="${number}" data-side="${side}" title="Comment on ${side} line ${number}"`:'';
  return `<div class="diff-line ${cls}"><span class="line-number">${left}</span><button type="button" class="line-number" ${action} ${!action?'disabled':''}>${right||'+'}</button><code>${esc(text)}</code></div>`;
 }).join('');
 return `<details class="diff-file" open><summary>${esc(f.path)} <span>${esc(f.status)}</span></summary><div class="diff-scroll">${rows}</div></details>`;
}
document.addEventListener('click',async e=>{
 const b=e.target.closest('[data-action^="remote-"]');if(!b)return;
 const action=b.dataset.action,m=list('merge_requests').find(m=>m.id===b.dataset.id);
 try{
  if(action==='remote-link'){
   modal('Link a provider merge request',`<div class="note">${esc(project().repository.full_name)} · Enter an existing GitHub pull request or GitLab merge request number. Connect your account before linking.</div><p><a class="button" href="/api/oauth/${esc(project().repository.provider)}/start?project_id=${encodeURIComponent(currentProject)}&review=1">Connect my provider account ↗</a></p>`+field('Request number','number','','number'),'Link request',async f=>{const result=await remoteCall('link',null,{number:Number(f.number)});view='mrs';selected=result.merge_request.id});return;
  }
  if(!m)return;
  if(action==='remote-sync'){b.disabled=true;await remoteCall('sync',m);await refresh();toast('Provider status refreshed');return}
  if(action==='remote-diff'){b.disabled=true;const result=await remoteCall('diff',m);diffCache.set(m.id+':'+result.head_sha+':'+m.remote.base_sha,result.files);await refresh();return}
  if(action==='remote-merge'){
   const {blocks}=reviewBlocks(m);
   modal('Merge into '+m.target,`<div class="note">This merges <strong>${esc(m.source)}</strong> into <strong>${esc(m.target)}</strong> in ${esc(project().repository.full_name)} at ${esc(project().repository.provider)}. GitHub uses a merge commit; GitLab uses the repository’s configured merge method.</div><p>Reviewed commit: <code>${esc(m.remote.head_sha)}</code></p>${blocks.length?`<ul>${blocks.map(b=>`<li>${esc(b)}</li>`).join('')}</ul>`:''}`,'Merge at provider',blocks.length?null:async()=>{await remoteCall('merge',m)});return;
  }
  if(action==='remote-approve'||action==='remote-changes'){
   const decision=action==='remote-approve'?'approve':'request_changes';
   modal(decision==='approve'?'Approve current commit':'Request changes',`<p>Commit <code>${esc(m.remote.head_sha.slice(0,12))}</code></p><label>Review<textarea name="body" ${decision==='request_changes'?'required':''}></textarea></label>`,'Submit review',f=>remoteCall('review',m,{decision,body:f.body}));return;
  }
  let anchor={};
  if(action==='remote-inline'){
   const files=diffCache.get(m.id+':'+m.remote.head_sha+':'+m.remote.base_sha),f=files?.[Number(b.dataset.file)];if(!f)throw Error('Reload the diff first');
   anchor={path:f.path,line:Number(b.dataset.line),side:b.dataset.side};
  }
  modal('Review comment',`${anchor.path?`<p>${esc(anchor.path)}:${anchor.line} (${anchor.side})</p>`:''}<label>Comment<textarea name="body" required maxlength="20000"></textarea></label>`,'Add comment',f=>remoteCall('comment',m,{body:f.body,...anchor}));
 }catch(error){toast(error.message);await refresh().catch(()=>{})}finally{b.disabled=false}
});
