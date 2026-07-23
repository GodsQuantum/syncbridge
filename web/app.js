
const $=s=>document.querySelector(s);
let JOBS=[], sw={backup:false,skipNew:false,sysBackup:false};
let paneState={src:"/mnt",dst:"/mnt"};

const ICON={
  play:'<svg viewBox="0 0 16 16" fill="none"><path d="M4 3l9 5-9 5V3z" fill="currentColor"/></svg>',
  eye:'<svg viewBox="0 0 16 16" fill="none"><path d="M1 8s2.5-4.5 7-4.5S15 8 15 8s-2.5 4.5-7 4.5S1 8 1 8z" stroke="currentColor" stroke-width="1.3"/><circle cx="8" cy="8" r="1.8" fill="currentColor"/></svg>',
  edit:'<svg viewBox="0 0 16 16" fill="none"><path d="M11 2l3 3-8 8H3v-3l8-8z" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round"/></svg>',
  trash:'<svg viewBox="0 0 16 16" fill="none"><path d="M3 4h10M6 4V2h4v2M5 4l1 9h4l1-9" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"/></svg>',
  clone:'<svg viewBox="0 0 16 16" fill="none"><rect x="5" y="5" width="8" height="8" rx="1.5" stroke="currentColor" stroke-width="1.3"/><path d="M3 10V4a1 1 0 011-1h6" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/></svg>',
  pause:'<svg viewBox="0 0 16 16" fill="none"><rect x="4" y="3" width="3" height="10" rx="1" fill="currentColor"/><rect x="9" y="3" width="3" height="10" rx="1" fill="currentColor"/></svg>',
  power:'<svg viewBox="0 0 16 16" fill="none"><path d="M8 2v6M4.5 4.5a5 5 0 107 0" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/></svg>',
  stop:'<svg viewBox="0 0 16 16" fill="none"><rect x="4" y="4" width="8" height="8" rx="1.5" fill="currentColor"/></svg>'
};

// Traducteur cron -> français. Gère les cas courants + expressions custom.
const CRON_PRESETS=[
  {label:"Toutes les 15 min", cron:"*/15 * * * *"},
  {label:"Toutes les heures", cron:"0 * * * *"},
  {label:"Toutes les 6 h", cron:"0 */6 * * *"},
  {label:"Chaque jour 3 h", cron:"0 3 * * *"},
  {label:"Chaque jour minuit", cron:"0 0 * * *"},
  {label:"Chaque nuit 4 h", cron:"0 4 * * *"},
  {label:"Lun-Ven 3 h", cron:"0 3 * * 1-5"},
  {label:"Dimanche 2 h", cron:"0 2 * * 0"},
  {label:"1er du mois 5 h", cron:"0 5 1 * *"},
];
const JOURS=["dimanche","lundi","mardi","mercredi","jeudi","vendredi","samedi"];
function cronHuman(c){
  if(!c) return "";
  const p=c.trim().split(/\s+/);
  if(p.length!==5) return c; // pas 5 champs -> affiche brut
  const [mi,h,dom,mo,dow]=p;
  // essaie une traduction lisible ; sinon renvoie l'expression
  const pad=n=>String(n).padStart(2,"0");
  const heureFixe=(/^\d+$/.test(h)&&/^\d+$/.test(mi))?`${pad(h)}:${pad(mi)}`:null;
  // fréquence minute
  if(mi.startsWith("*/")&&h==="*"&&dom==="*"&&mo==="*"&&dow==="*")
    return `toutes les ${mi.slice(2)} min`;
  if(mi==="0"&&h==="*"&&dom==="*"&&mo==="*"&&dow==="*") return "toutes les heures";
  if(mi==="0"&&h.startsWith("*/")&&dom==="*"&&dow==="*") return `toutes les ${h.slice(2)} h`;
  // jour de semaine
  let quand="";
  if(dow==="1-5") quand="du lundi au vendredi";
  else if(dow==="0"||dow==="7") quand="le dimanche";
  else if(dow==="6") quand="le samedi";
  else if(/^\d$/.test(dow)) quand="le "+JOURS[+dow%7];
  else if(dow==="*"&&dom==="*") quand="chaque jour";
  else if(/^\d+$/.test(dom)) quand=`le ${dom} du mois`;
  else quand="";
  if(heureFixe&&quand) return `${quand} à ${heureFixe}`;
  if(heureFixe) return `chaque jour à ${heureFixe}`;
  return c; // fallback : expression brute
}
// validation grossière (5 champs)
function cronValid(c){return c.trim().split(/\s+/).length===5;}

async function boot(){
  // Dispo des moteurs : sert juste à griser rclone dans l'éditeur s'il n'est pas installé.
  try{
    const e=await (await fetch("/api/engines")).json();
    const opt=$("#f-engine");
    if(!e.rclone && opt){ const o=opt.querySelector('[value=rclone]'); if(o) o.disabled=true; }
  }catch(e){}
  await load();
  setInterval(refreshLive, 2000); // suivi live toutes les 2s
}
async function load(){ JOBS=await (await fetch("/api/jobs")).json(); render(); }

// --- suivi live : progression %, durée, ETA, prochain run ---
function fmtDur(sec){
  sec=Math.max(0,Math.round(sec));
  const h=Math.floor(sec/3600), m=Math.floor((sec%3600)/60), s=sec%60;
  if(h>0)return `${h}h${String(m).padStart(2,"0")}`;
  if(m>0)return `${m}min${String(s).padStart(2,"0")}`;
  return `${s}s`;
}
function fmtNext(iso){
  if(!iso)return "";
  const d=new Date(iso), now=new Date();
  const j=["dim","lun","mar","mer","jeu","ven","sam"][d.getDay()];
  const date=`${j} ${String(d.getDate()).padStart(2,"0")}/${String(d.getMonth()+1).padStart(2,"0")}`;
  const heure=`${String(d.getHours()).padStart(2,"0")}:${String(d.getMinutes()).padStart(2,"0")}`;
  // délai relatif
  const diff=(d-now)/1000;
  let rel="";
  if(diff>0){ if(diff<3600)rel=` (dans ${Math.round(diff/60)}min)`; else if(diff<86400)rel=` (dans ${Math.round(diff/3600)}h)`; else rel=` (dans ${Math.round(diff/86400)}j)`; }
  return `⏭ prochain : ${date} ${heure}${rel}`;
}
// Parse une ligne rsync --info=progress2.
// Piège : pendant le SCAN (ir-chk présent), le % est faux (total pas encore connu).
// On ne fait confiance au % qu'une fois le scan fini (to-chk, ou plus de ir-chk).
function parseProgress(line){
  if(!line)return null;
  const scanning=/ir-chk=/.test(line);        // rsync liste encore les fichiers
  const pctM=line.match(/(\d+)%/);
  const speedM=line.match(/([\d.]+\s?[kMGT]?B\/s)/);
  const etaM=line.match(/(\d+:\d\d:\d\d)/);
  const xfrM=line.match(/xfr#(\d+)/);
  const tochkM=line.match(/to-chk=(\d+)\/(\d+)/);
  let pct=pctM?+pctM[1]:null, eta=etaM?etaM[1]:null;
  // pendant le scan, le % et l'ETA sont trompeurs -> on les ignore
  if(scanning){ pct=null; eta=null; }
  // si to-chk dispo (fin de scan), on peut calculer un % fiable sur le nb de fichiers
  let filePct=null;
  if(tochkM){ const rest=+tochkM[1], tot=+tochkM[2]; if(tot>0) filePct=Math.round((tot-rest)/tot*100); }
  return {
    scanning, pct, eta,
    speed:speedM?speedM[1].replace(/\s/,""):null,
    xfr:xfrM?+xfrM[1]:null,
    filePct, // % basé sur les fichiers restants (plus fiable en fin de job)
  };
}
async function refreshLive(){
  let sts;
  try{ sts=await (await fetch("/api/status")).json(); }catch(e){ return; }
  sts.forEach(s=>{
    // prochain run
    const nx=document.querySelector(`#next-${s.id}`);
    if(nx)nx.textContent=(!s.running&&s.nextRun)?fmtNext(s.nextRun):"";
    // barre live
    const bar=document.querySelector(`#live-${s.id}`); if(!bar)return;
    const killBtn=document.querySelector(`#kill-${s.id}`);
    if(killBtn)killBtn.style.display=s.running?"inline-flex":"none";
    const dot=document.querySelector(`#c-${s.id}`)?.closest(".job")?.querySelector(".dot");
    if(s.running){
      if(dot)dot.className="dot running";
      const p=parseProgress(s.progress);
      const dur=fmtDur(s.since);
      // % à afficher : priorité au filePct (fin de scan, fiable), sinon pct transfert
      let shownPct = p ? (p.filePct!=null ? p.filePct : p.pct) : null;
      let etaTxt = (p&&p.eta&&!p.scanning) ? `<span class="eta">reste ~${p.eta}</span>` : "";
      let spd = (p&&p.speed&&p.speed!=="0.00kB/s") ? `<span class="chip">${p.speed}</span>` : "";
      let headTxt;
      if(!p || p.scanning){
        // phase de scan : pas de % fiable
        headTxt=`<span class="pct scan">analyse des fichiers…</span><span class="chip">⏱ ${dur}</span>`;
      }else if(shownPct!=null){
        headTxt=`<span class="pct">${shownPct}%</span><span class="chip">⏱ ${dur}</span>${spd}${etaTxt}`;
      }else{
        headTxt=`<span class="pct">transfert…</span><span class="chip">⏱ ${dur}</span>${spd}`;
      }
      const indeterminate = (!p || p.scanning || shownPct==null);
      bar.className="live-bar active";
      bar.innerHTML=`
        <div class="live-head">${headTxt}</div>
        <div class="progress-track">
          <div class="progress-fill ${indeterminate?'indeterminate':''}" style="width:${indeterminate?0:shownPct}%"></div>
        </div>`;
    }else{
      bar.className="live-bar";
      bar.innerHTML="";
    }
  });
}

let FILTER="all";
const TABS=[
  {id:"all",label:"Tous",match:j=>true},
  {id:"sync",label:"Sync",match:j=>j.kind!=="command"},
  {id:"command",label:"Commandes",match:j=>j.kind==="command"},
  {id:"cron",label:"⏰ Cron",match:j=>j.trigger==="cron"},
  {id:"watch",label:"👀 Watch",match:j=>j.trigger==="watch"},
  {id:"manual",label:"🖐 Manuel",match:j=>j.trigger==="manual"},
  {id:"disabled",label:"Désactivés",match:j=>!!j.disabled}
];
function setFilter(id){FILTER=id;render();}
function renderTabs(){
  const box=$("#tabs"); if(!box)return;
  box.innerHTML=TABS.map(t=>{
    const n=JOBS.filter(t.match).length;
    return `<span class="tab ${FILTER===t.id?'sel':''}" onclick="setFilter('${t.id}')"><span class="tlbl">${t.label}</span><span class="cnt">${n}</span></span>`;
  }).join("");
}
function render(){
  renderTabs();
  const box=$("#jobs");
  const q=($("#search")?$("#search").value:"").toLowerCase().trim();
  const tab=TABS.find(t=>t.id===FILTER)||TABS[0];
  const list=JOBS.filter(tab.match).filter(j=>!q||[j.name,j.command,j.source,j.dest].some(v=>(v||"").toLowerCase().includes(q)));
  if(!JOBS.length){
    box.innerHTML=`<div class="empty"><div class="big">⇄</div>
      <b>Aucun job pour l'instant</b>
      <p>Crée ta première tâche : choisis un dossier source, un dossier destination, un comportement, et une fréquence.</p>
      <button class="btn pri" onclick="openEditor()">Créer un job</button></div>`;
    return;
  }
  if(!list.length){ box.innerHTML=`<div class="empty" style="padding:40px"><b>Aucun job dans ce filtre</b><p style="font-size:12px">Change d'onglet ou crée un job.</p></div>`; return; }
  box.innerHTML=list.map(j=>{
    const st=j.lastStat||"", dot=st==="running"?"running":st==="ok"?"ok":st==="error"?"error":"";
    const isCmd=j.kind==="command";
    const mt={add:"add",mirror:"mirror",move:"move"}[j.mode];
    const ml={add:"accumulation",mirror:"miroir",move:"déplacement"}[j.mode];
    const trig=j.trigger==="cron"?cronHuman(j.cron):j.trigger==="watch"?(j.watchGlob?"surveille "+esc(j.watchGlob):"constant"):"manuel";
    const dis=j.disabled;
    const flow=isCmd
      ? `<div class="flow"><span class="p">$ ${esc(j.command||"")}</span></div>`
      : `<div class="flow"><span class="p">${esc(j.source)}</span><span class="ar">→</span><span class="p">${esc(j.dest)}</span></div>`;
    const tags=isCmd
      ? `<span class="tag eng">commande</span><span class="tag trig">${trig}</span>`+(j.trigger==="watch"&&j.source?'<span class="tag">'+esc(j.source)+'</span>':'')
      : `<span class="tag ${mt}"><b>${ml}</b></span><span class="tag trig">${trig}</span><span class="tag eng">${j.engine}</span>`
        +(j.compare==='checksum'?'<span class="tag">checksum</span>':'')
        +(j.sysBackup?'<span class="tag sys">🔒 système</span>':'')
        +(j.backup?'<span class="tag">corbeille'+(j.backupKeep>0?' ×'+j.backupKeep:'')+'</span>':'')
        +(j.bwlimit?'<span class="tag">'+esc(j.bwlimit)+'/s</span>':'');
    return `<div class="job ${dis?'disabled':''}">
      <div class="job-row">
        <span class="dot ${dis?'':dot}"></span>
        <div class="job-body">
          <div class="job-title">${esc(j.name)}${dis?' <span class="off-lbl">désactivé</span>':''}</div>
          ${flow}
          <div class="tags">
            ${tags}
          </div>
        </div>
        <div class="job-act">
          <button class="btn sm ghost ${dis?'on':''}" onclick="toggle(${j.id})" title="${dis?'Réactiver':'Désactiver'}">${dis?ICON.power:ICON.pause}</button>
          ${isCmd?'':`<button class="btn sm ghost" onclick="run(${j.id},1)" title="Simuler (dry-run)">${ICON.eye}</button>`}
          <button class="btn sm pri" onclick="run(${j.id},0)">${ICON.play} Lancer maintenant</button>
          <button class="btn sm kill" id="kill-${j.id}" onclick="killJob(${j.id})" style="display:none" title="Arrêter le run en cours">${ICON.stop} Kill</button>
          <button class="btn sm ghost" onclick="edit(${j.id})" title="Éditer">${ICON.edit}</button>
          <button class="btn sm ghost" onclick="clone(${j.id})" title="Cloner">${ICON.clone}</button>
          <button class="btn sm ghost dgr" onclick="del(${j.id})" title="Supprimer">${ICON.trash}</button>
        </div>
      </div>
      <div class="job-meta">
        <span>dernier : ${j.lastRun||"jamais"}</span>
        <span>statut : ${st||"—"}</span>
        <span class="next-run" id="next-${j.id}"></span>
      </div>
      <div class="live-bar" id="live-${j.id}"></div>
      <div class="console" id="c-${j.id}"></div>
    </div>`;
  }).join("");
  refreshLive(); // remplit le suivi live immédiatement après un render
}
function esc(s){return (s||"").replace(/[&<>"]/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;"}[c]));}

// ---- run + stream ----
async function run(id,dry){
  const c=$(`#c-${id}`); c.innerHTML=""; c.style.display="block";
  const r=await fetch(`/api/jobs/${id}/run?dry=${dry}`,{method:"POST"});
  if(!r.ok){toast(await r.text(),"err");return;}
  const es=new EventSource(`/api/jobs/${id}/stream`);
  es.onmessage=e=>{
    const l=JSON.parse(e.data), d=document.createElement("div");
    if(l.startsWith("===")||l.startsWith("$"))d.className="cmd";
    else if(l.startsWith("•")||l.startsWith("  "))d.className="sum";
    else if(/ERREUR|error|failed/i.test(l))d.className="err";
    else if(l.startsWith("=== terminé"))d.className="done";
    else if(/deleting/.test(l))d.className="del";
    d.textContent=l; c.appendChild(d); c.scrollTop=c.scrollHeight;
  };
  es.addEventListener("done",()=>{es.close();refreshStatus(id);}); // PAS load() : garde la console
  es.onerror=()=>es.close();
}
// Met à jour seulement le point de statut + la meta d'un job, sans re-render (console préservée).
async function refreshStatus(id){
  const list=await (await fetch("/api/jobs")).json();
  const j=list.find(x=>x.id===id); if(!j)return;
  JOBS=list; // garde les données à jour pour les autres actions
  const card=document.querySelector(`#c-${id}`)?.closest(".job"); if(!card)return;
  const st=j.lastStat||"";
  const dot=card.querySelector(".dot");
  if(dot&&!j.disabled)dot.className="dot "+(st==="running"?"running":st==="ok"?"ok":st==="error"?"error":"");
  const meta=card.querySelector(".job-meta");
  if(meta)meta.innerHTML=`<span>dernier : ${j.lastRun||"jamais"}</span><span>statut : ${st||"—"}</span>`;
}
async function del(id){
  if(!confirm("Supprimer ce job ? Aucun fichier n'est touché."))return;
  await fetch(`/api/jobs/${id}`,{method:"DELETE"}); load(); toast("Job supprimé","ok");
}
async function clone(id){
  const r=await fetch(`/api/jobs/${id}/clone`,{method:"POST"});
  if(!r.ok)return toast("Échec du clonage","err");
  load(); toast("Job cloné (désactivé par sécurité)","ok");
}
async function toggle(id){
  const r=await fetch(`/api/jobs/${id}/toggle`,{method:"POST"});
  if(!r.ok)return toast("Échec","err");
  const d=await r.json(); load(); toast(d.disabled?"Job désactivé":"Job réactivé","ok");
}
async function killJob(id){
  if(!confirm("Arrêter le run en cours de ce job ?\nLes fichiers déjà copiés restent, rien n'est supprimé."))return;
  const r=await fetch(`/api/jobs/${id}/kill`,{method:"POST"});
  if(!r.ok)return toast("Échec de l'arrêt","err");
  const d=await r.json();
  toast(d.killed?"Run arrêté":"Aucun run actif à arrêter","ok");
  refreshLive();
}

// ---- import ----
function openImport(){ $("#import-scrim").classList.add("open"); scanImport(); }
function closeImport(){ $("#import-scrim").classList.remove("open"); }
async function scanImport(){
  const box=$("#import-list");
  box.innerHTML='<div class="hint">Scan du système en cours…</div>';
  let sys={items:[]}, imp={found:[]};
  try{ sys=await (await fetch("/api/system/scan")).json(); }catch(e){}
  try{ imp=await (await fetch("/api/import/scan")).json(); }catch(e){}
  const items=sys.items||[], found=imp.found||[];
  if(!items.length && !found.length){
    box.innerHTML=`<div class="empty" style="padding:32px">
      <b>Rien détecté sur le système</b>
      <p style="font-size:12px">Aucune tâche cron, unité systemd ou watcher inotify trouvé. Vérifie que les montages /host/… et /import/… sont présents (voir compose).</p></div>`;
    return;
  }
  const customs=items.filter(i=>i.class!=="system");
  const systems=items.filter(i=>i.class==="system");
  let html="";
  // 1) Tes déclencheurs (scripts perso, cron perso, inotify) + copies rsync/rclone
  if(customs.length||found.length){
    html+=`<div class="section-h" style="margin:0 0 10px">Tes déclencheurs<span class="sh-sub"> — scripts, cron perso, inotify</span></div>`;
    html+=`<div class="hint" style="margin:0 0 12px">Ceux que tu gères toi-même. Importe-les dans SyncBridge pour les piloter proprement (créés désactivés — ton système n'est pas touché).</div>`;
    html+=customs.map(it=>renderSysItem(it,false)).join("");
    html+=found.map(renderFound).join("");
  }
  // 2) Déclencheurs système (OS & paquets) : à ne toucher qu'à ses risques
  if(systems.length){
    html+=`<div class="section-h" style="margin:24px 0 10px">Déclencheurs système<span class="sh-sub"> — OS & paquets</span></div>`;
    html+=`<div class="sys-warn">⚠ Tâches gérées par l'OS et les paquets (ex. <code>run-parts /etc/cron.hourly</code>, <code>e2scrub_all_cron</code>). Pratique pour <b>visualiser</b>, mais à <b>ne modifier qu'à tes risques et périls</b> : les désactiver peut casser des mises à jour, du nettoyage disque ou de la maintenance système.</div>`;
    html+=systems.map(it=>renderSysItem(it,true)).join("");
  }
  box.innerHTML=html;
}
const SYS_TYPE={"cron":["⏰","cron"],"systemd-service":["⚙️","systemd service"],"systemd-timer":["⚙️","systemd timer"],"systemd-path":["👀","systemd .path"],"inotify-proc":["👀","inotify"]};
function renderSysItem(it,isSys){
  const t=SYS_TYPE[it.type]||["•",it.type];
  const managed=it.managed?'<div class="imp-warn" style="color:var(--green)">✓ déjà géré par SyncBridge</div>':'';
  const J=JSON.stringify(it).replace(/'/g,"&#39;");
  let sysBtn="";
  if(!it.managed){
    if(it.type==="inotify-proc"){
      sysBtn=`<button class="btn sm dgr" onclick='sysKill(${J})' title="Tuer le process (non réversible)">Arrêter (kill)</button>`;
    }else{
      sysBtn = it.disabled
        ? `<button class="btn sm" style="color:var(--green);border-color:#2b5c42" onclick='sysToggle(${J})'>Réactiver sur le système</button>`
        : `<button class="btn sm" style="color:var(--amber);border-color:#5b4a26" onclick='sysToggle(${J})'>Désactiver sur le système</button>`;
    }
  }
  const offLbl=it.disabled?' <span class="off-lbl">désactivé (système)</span>':'';
  return `<div class="imp-item ${isSys?'sys':''}" ${it.disabled?'style="opacity:.72"':''}>
    <div class="imp-flow">
      <span class="imp-eng">${t[0]} ${t[1]}</span>
      <span class="p">${esc(it.name||"")}</span>
      ${it.schedule?`<span class="p">${esc(it.schedule)}</span>`:""}${offLbl}
    </div>
    <div class="imp-meta">
      ${it.target?`<span>→ <b>${esc(it.target)}</b></span>`:'<span>commande non extraite (à compléter)</span>'}
      <span class="imp-file">${esc((it.file||"").split("/").pop())}</span>
    </div>
    ${managed}
    <div class="imp-actions">
      <button class="btn sm ${isSys?'':'pri'}" onclick='importSys(${J})'>Importer${isSys?' (visualiser)':''}</button>
      ${sysBtn}
    </div>
  </div>`;
}
function renderFound(f){
  const modeGuess=/--delete/.test(f.line)||f.verb==="sync"?"miroir":f.verb==="move"?"déplacement":"accumulation";
  return `<div class="imp-item ${f.local?'':'warn'}">
    <div class="imp-flow">
      <span class="imp-eng">${f.engine}${f.verb?" "+f.verb:""}</span>
      <span class="p">${esc(f.source)}</span><span class="arrow">→</span><span class="p">${esc(f.dest)}</span>
    </div>
    <div class="imp-meta">
      <span>mode probable : <b>${modeGuess}</b></span>
      ${f.cron?`<span>cron : <b>${esc(f.cron)}</b></span>`:'<span>pas de planning détecté</span>'}
      <span class="imp-file">${esc(f.file.split("/").pop())}</span>
    </div>
    ${f.warning?`<div class="imp-warn">⚠ ${esc(f.warning)}</div>`:''}
    <div class="imp-actions">
      <button class="btn sm ${f.local?'pri':''}" onclick='importOne(${JSON.stringify(f).replace(/'/g,"&#39;")},"${modeGuess}")'>Importer comme synchro</button>
    </div>
  </div>`;
}
async function sysToggle(it){
  const disabling=!it.disabled;
  const msg=disabling
    ? "Désactiver ce déclencheur sur le SYSTÈME ?\n\nRéversible : la ligne cron est commentée (#SB-OFF#) ou l'unité systemd passée en 'disable'. Tu pourras la réactiver ici."
    : "Réactiver ce déclencheur sur le système ?";
  if(!confirm(msg))return;
  const r=await fetch("/api/system/toggle",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(it)});
  if(!r.ok)return toast(await r.text(),"err");
  const d=await r.json();
  toast(d.state==="disabled"?"Désactivé sur le système":d.state==="enabled"?"Réactivé sur le système":"Fait","ok");
  scanImport();
}
async function sysKill(it){
  if(!confirm("Arrêter ce process inotify (SIGTERM) ?\n\n⚠ NON réversible : le process est tué. Il repartira au prochain (re)démarrage de son service parent."))return;
  const r=await fetch("/api/system/toggle",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(it)});
  if(!r.ok)return toast(await r.text(),"err");
  toast("Process arrêté","ok"); scanImport();
}
async function logout(){ try{ await fetch("/api/auth/logout",{method:"POST"}); }catch(e){} location.replace("/"); }
function importSys(it){
  closeImport(); openEditor();
  $("#mtitle").textContent="Importer un job";
  setKind("command");
  $("#f-name").value=it.name||("Import "+(it.type||"job"));
  $("#f-command").value=it.target||"";
  if(it.type==="cron" && it.schedule && it.schedule.trim().split(/\s+/).length===5){
    setTrig("cron"); $("#f-cron").value=it.schedule; updateCronLive(); highlightCronChip();
  }else if(it.type==="systemd-path"||it.type==="inotify-proc"){
    setTrig("watch"); $("#f-source").value=it.schedule||""; $("#f-watchsrc").value=it.schedule||"";
  }else{
    setTrig("manual");
  }
  toast("Vérifie puis enregistre. Le job sera créé désactivé — ton système reste inchangé.","ok");
}
async function importOne(f,mode){
  // ouvre l'éditeur pré-rempli plutôt que créer aveuglément -> tu vérifies
  closeImport();
  openEditor();
  $("#mtitle").textContent="Importer un job";
  $("#f-name").value=`Import ${f.source.split("/").filter(Boolean).pop()||"copie"}`;
  $("#f-source").value=f.source.replace(/\/$/,""); $("#src-mini").textContent=f.source;
  $("#f-dest").value=f.dest.replace(/\/$/,""); $("#dst-mini").textContent=f.dest;
  $("#f-engine").value=f.engine==="rclone"?"rclone":"rsync";
  setMode(mode==="miroir"?"mirror":mode==="déplacement"?"move":"add");
  if(f.cron){ setTrig("cron"); $("#f-cron").value=f.cron; updateCronLive(); highlightCronChip(); }
  else setTrig("manual");
  toast("Vérifie les réglages puis enregistre. Le job sera créé désactivé.","ok");
}

// ---- editor ----
function openEditor(){
  $("#mtitle").textContent="Nouveau job";
  ["f-id","f-name","f-command","f-source","f-dest","f-cron","f-watchglob","f-watchsrc","f-debounce","f-pollsec","f-timeout","f-bwlimit","f-maxdel","f-exclude","f-backupkeep"].forEach(i=>$("#"+i).value="");
  $("#src-mini").textContent="—"; $("#dst-mini").textContent="—";
  $("#f-engine").value="rsync"; $("#f-compare").value="time"; $("#f-watchmode").value="hybrid";
  setKind("sync"); setBackend("syncbridge"); setMode("add"); setTrig("manual"); sw={backup:false,skipNew:false,sysBackup:false}; syncSw();
  $("#adv").classList.remove("open"); $("#adv-toggle").classList.remove("open");
  paneState={src:"/mnt",dst:"/mnt"}; loadPane("src"); loadPane("dst");
  $("#scrim").classList.add("open");
}
function edit(id){
  const j=JOBS.find(x=>x.id===id); if(!j)return;
  $("#mtitle").textContent="Éditer le job";
  $("#f-id").value=j.id; $("#f-name").value=j.name;
  $("#f-command").value=j.command||"";
  $("#f-source").value=j.source; $("#f-dest").value=j.dest;
  $("#src-mini").textContent=j.source; $("#dst-mini").textContent=j.dest;
  $("#f-engine").value=j.engine; $("#f-compare").value=j.compare;
  $("#f-cron").value=j.cron||""; $("#f-bwlimit").value=j.bwlimit||"";
  $("#f-watchglob").value=j.watchGlob||""; $("#f-watchmode").value=j.watchMode||"hybrid"; $("#f-watchsrc").value=j.source||"";
  $("#f-debounce").value=j.debounce||""; $("#f-pollsec").value=j.pollSec||"";
  $("#f-timeout").value=j.timeout||"";
  $("#f-maxdel").value=j.maxDel||""; $("#f-exclude").value=j.exclude||"";
  $("#f-backupkeep").value=j.backupKeep||"";
  setKind(j.kind||"sync"); setBackend(j.backend||"syncbridge"); setMode(j.mode); setTrig(j.trigger);
  sw={backup:!!j.backup,skipNew:!!j.skipNew,sysBackup:!!j.sysBackup}; syncSw();
  const anyAdv=j.sysBackup||j.backup||j.skipNew||j.bwlimit||j.maxDel||j.exclude;
  $("#adv").classList.toggle("open",!!anyAdv); $("#adv-toggle").classList.toggle("open",!!anyAdv);
  paneState={src:j.source||"/mnt",dst:j.dest||"/mnt"}; loadPane("src"); loadPane("dst");
  $("#scrim").classList.add("open");
}
function closeEditor(){$("#scrim").classList.remove("open");}

function setMode(m){$("#f-mode").value=m;
  ["add","mirror","move"].forEach(x=>$("#m-"+x).classList.toggle("sel",x===m));}
function setKind(k){$("#f-kind").value=k;
  ["sync","command"].forEach(x=>$("#k-"+x).classList.toggle("sel",x===k));
  const cmd=k==="command";
  $("#cmd-only").style.display=cmd?"block":"none";
  document.querySelectorAll(".sync-only").forEach(el=>el.style.display=cmd?"none":"");
  updateWatchSrc();}
function setBackend(b){$("#f-backend").value=b;
  ["syncbridge","system"].forEach(x=>$("#b-"+x).classList.toggle("sel",x===b));}
function updateWatchSrc(){const el=$("#watch-src-wrap"); if(el) el.style.display=($("#f-kind").value==="command"&&$("#f-trigger").value==="watch")?"block":"none";}
function setTrig(t){$("#f-trigger").value=t;
  ["manual","cron","watch"].forEach(x=>$("#t-"+x).classList.toggle("sel",x===t));
  $("#cron-wrap").style.display=t==="cron"?"block":"none";
  $("#watch-wrap").style.display=t==="watch"?"block":"none";
  if(t==="cron"){renderCronPresets();updateCronLive();highlightCronChip();}
  updateWatchSrc();}
function toggleAdv(){$("#adv").classList.toggle("open");$("#adv-toggle").classList.toggle("open");}
function toggleSw(k){sw[k]=!sw[k];syncSw();}
function syncSw(){$("#sw-backup").classList.toggle("on",sw.backup);$("#sw-skipnew").classList.toggle("on",sw.skipNew);
  $("#sw-sysbackup").classList.toggle("on",sw.sysBackup);
  $("#keep-wrap").style.display=sw.backup?"block":"none";}
// --- cron builder ---
function renderCronPresets(){
  const box=$("#cron-presets"); if(!box)return;
  box.innerHTML=CRON_PRESETS.map(p=>
    `<span class="cron-chip" data-cron="${p.cron}" onclick="pickCron('${p.cron}')">${p.label}</span>`
  ).join("");
}
function pickCron(c){ $("#f-cron").value=c; updateCronLive(); highlightCronChip(); }
function highlightCronChip(){
  const cur=$("#f-cron").value.trim();
  document.querySelectorAll(".cron-chip").forEach(ch=>
    ch.classList.toggle("sel",ch.dataset.cron===cur));
}
function updateCronLive(){
  const v=$("#f-cron").value.trim(), live=$("#cron-live");
  if(!v){live.className="cron-live";live.textContent="↳ choisis un préréglage ou tape une expression";return;}
  if(!cronValid(v)){live.className="cron-live bad";live.textContent="⚠ il faut 5 champs : min heure jour mois jour-semaine";return;}
  const h=cronHuman(v);
  live.className="cron-live";
  live.innerHTML="↳ <b>"+(h===v?"expression personnalisée":h)+"</b>";
}
$("#f-cron").addEventListener("input",()=>{updateCronLive();highlightCronChip();});

async function saveJob(){
  const kind=$("#f-kind").value;
  const p={
    name:$("#f-name").value.trim(),kind:kind,command:$("#f-command").value.trim(),
    backend:$("#f-backend").value,timeout:parseInt($("#f-timeout").value)||0,
    source:$("#f-source").value.trim(),dest:$("#f-dest").value.trim(),
    engine:$("#f-engine").value,mode:$("#f-mode").value,trigger:$("#f-trigger").value,
    cron:$("#f-cron").value.trim(),compare:$("#f-compare").value,
    watchGlob:$("#f-watchglob").value.trim(),watchMode:$("#f-watchmode").value,
    debounce:parseInt($("#f-debounce").value)||0,pollSec:parseInt($("#f-pollsec").value)||0,
    bwlimit:$("#f-bwlimit").value.trim(),maxDel:parseInt($("#f-maxdel").value)||0,
    backupKeep:parseInt($("#f-backupkeep").value)||0,
    backup:sw.backup,skipNew:sw.skipNew,sysBackup:sw.sysBackup,exclude:$("#f-exclude").value.trim()
  };
  if(kind==="command"&&p.trigger==="watch") p.source=$("#f-watchsrc").value.trim();
  if(!p.name)return toast("Nom requis","err");
  if(kind==="command"){ if(!p.command)return toast("Renseigne la commande à exécuter","err"); }
  else if(!p.source||!p.dest)return toast("Source et destination requises","err");
  if(p.trigger==="cron"&&!p.cron)return toast("Renseigne l'expression cron","err");
  const id=$("#f-id").value;
  const r=await fetch(id?`/api/jobs/${id}`:"/api/jobs",
    {method:id?"PUT":"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(p)});
  if(!r.ok)return toast(await r.text(),"err");
  closeEditor(); load(); toast(id?`Job « ${p.name} » modifié`:`Job « ${p.name} » créé`,"ok");
}

// ---- dual pane browse ----
async function loadPane(side){
  try{
    const r=await fetch(`/api/browse?path=${encodeURIComponent(paneState[side])}`);
    if(!r.ok){paneState[side]="/mnt";return loadPane(side);}
    const d=await r.json(); paneState[side]=d.path;
    $(`#${side}-cur`).innerHTML="<b>"+esc(d.path)+"</b>";
    let h="";
    if(d.parent)h+=`<div class="pane-item up ${side}-i" onclick="cd('${side}','${escA(d.parent)}')"><span class="ic">↰</span> ..</div>`;
    h+=d.dirs.map(n=>{const np=d.path==="/"?"/"+n:d.path+"/"+n;
      return `<div class="pane-item ${side}-i" onclick="cd('${side}','${escA(np)}')"><span class="ic">▸</span> ${esc(n)}</div>`;}).join("");
    $(`#${side}-list`).innerHTML=h||`<div style="padding:12px;color:var(--dim);font-family:var(--mono);font-size:12px">vide</div>`;
  }catch(e){toast("Accès dossier refusé","err");}
}
function escA(s){return s.replace(/'/g,"\\'");}
function cd(side,p){paneState[side]=p;loadPane(side);}
function pick(side){
  const p=paneState[side];
  $(`#f-${side==='src'?'source':'dest'}`).value=p;
  $(`#${side}-mini`).textContent=p;
  toast((side==='src'?'Source':'Destination')+" : "+p,"ok");
}

let tT;
function toast(m,t){const el=$("#toast");el.textContent=m;el.className="toast show "+(t||"");
  clearTimeout(tT);tT=setTimeout(()=>el.className="toast",2600);}

// ---- i18n : anglais par défaut, bascule française. Couche NON destructive :
// on ne touche pas au HTML français (qui reste la source), on le traduit à la volée. ----
const I18N_EN = {
  "moteurs":"engines","Nouveau job":"New job","Jobs de synchronisation":"Sync jobs","⤵ Importer":"⤵ Import",
  "Nom":"Name","Type de job":"Job type","Synchronisation":"Sync",
  "Copie un dossier A → B (rsync/rclone). Le cas d'origine.":"Copy a folder A → B (rsync/rclone). The original use case.",
  "Commande / script":"Command / script",
  "Lance une commande ou un script (ex. docker system prune -f, /import/scripts/CRON/DIAG-LIGHT.sh). Déclenchable en manuel, cron ou surveillance.":"Runs a command or script (e.g. <b>docker system prune -f</b>, <b>/import/scripts/cron/diag.sh</b>). Trigger it manually, on cron, or on a folder change.",
  "Commande à exécuter":"Command to run",
  "Exécutée via sh -c dans le conteneur. stdout/stderr capturés et streamés dans les logs (bouton « Lancer »). Job Docker : la socket doit être montée (voir compose).":"Runs via <b>sh -c</b> in the container. <b>stdout/stderr</b> are captured and streamed to the logs (Run button). For Docker jobs, mount the socket (see compose).",
  "Exécution":"Execution","Backend SyncBridge":"SyncBridge backend",
  "SyncBridge exécute le job : logs en direct, bouton d'arrêt, verrou anti-chevauchement. S'arrête si le conteneur s'arrête.":"SyncBridge runs the job: live logs, kill button, anti-overlap lock. Stops if the container stops.",
  "Backend Système":"System backend",
  "Écrit un vrai cron/systemd sur l'hôte : le job survit à une panne Docker. Demande un déclencheur cron ou surveillance (pas manuel) + des montages hôte en écriture. Pas de logs live.":"Writes a real host cron/systemd entry: the job <b>survives a Docker outage</b>. Needs a cron or watch trigger (not manual) + read-write host mounts. No live logs.",
  "Délai max d'exécution (s)":"Max run time (s)",
  "Tue le job s'il dépasse ce temps (anti-blocage). 0 = pas de limite.":"Kills the job if it runs longer than this (anti-hang). <b>0</b> = no limit.",
  "Dossiers":"Folders","Source (A)":"Source (A)","Destination (B)":"Destination (B)","Choisir":"Pick",
  "C'est le contenu de la source qui est copié dans la destination (pas le dossier lui-même).":"It's the <b>contents</b> of the source that get copied into the destination (not the folder itself).",
  "Comportement de copie":"Copy behavior","Accumulation":"Accumulate",
  "Ajoute les nouveaux fichiers et met à jour les modifiés. Ne supprime jamais dans B. Le plus sûr.":"Adds new files and updates changed ones. <b>Never deletes</b> in B. Safest.",
  "Miroir exact":"Exact mirror",
  "B devient une copie identique de A. Supprime dans B ce qui a disparu de A. Attention aux suppressions.":"B becomes an identical copy of A. <b>Deletes from B</b> whatever disappeared from A. Mind the deletions.",
  "Déplacement":"Move",
  "Copie vers B puis vide la source A. Pour évacuer un dossier de transit.":"Copies to B then <b>empties source A</b>. For draining a transit folder.",
  "Fréquence":"Frequency","Manuel":"Manual",
  "Se lance uniquement quand tu cliques « Lancer maintenant ».":"Runs only when you click \"Run now\".",
  "Planifié":"Scheduled","Se lance à heure fixe via une expression cron.":"Runs at a fixed time via a cron expression.",
  "Constant":"Constant",
  "Surveille A en continu et sync dès qu'un fichier change (attend 3 s de calme).":"Watches A continuously and syncs <b>as soon as a file changes</b> (waits for a quiet period).",
  "↳ choisis un préréglage ou tape une expression":"↳ pick a preset or type an expression",
  "Format : min heure jour mois jour-semaine. Le * = \"chaque\".":"Format: <b>min hour day month weekday</b>. <b>*</b> means \"every\".",
  "Filtre (motif)":"Filter (pattern)",
  "Ne déclenche que si un fichier correspond. Vide = tout. Sépare par des virgules.":"Only fires if a file matches. Empty = everything. Comma-separated.",
  "Mode de surveillance":"Watch mode",
  "Sur un montage NFS, inotify ne voit pas les écritures distantes : garde Hybride ou Scan.":"On an <b>NFS</b> mount, inotify can't see remote writes: keep <b>Hybrid</b> or <b>Scan</b>.",
  "Anti-rebond (s)":"Debounce (s)",
  "Attend N s de calme après le dernier changement avant de lancer. Coalesce les rafales.":"Waits N s of quiet after the last change before running. Coalesces bursts.",
  "Intervalle de scan (s)":"Scan interval (s)",
  "Fréquence du scan (modes Hybride/Scan). 300 = 5 min, comme un watcher classique.":"Scan frequency (Hybrid/Scan modes). 300 = 5 min, like a classic watcher.",
  "Moteur":"Engine","Méthode de comparaison":"Comparison method",
  "rsync — rapide, idéal disque local":"rsync — fast, ideal for local disks","rclone — plus de contrôle, remotes cloud":"rclone — more control, cloud remotes",
  "Taille + date — rapide (défaut)":"Size + date — fast (default)","Checksum — sûr, plus lent":"Checksum — safe, slower",
  "Hybride : événements + scan (recommandé, NFS)":"Hybrid: events + scan (recommended, NFS)","Événements : inotify seul (disque local rapide)":"Events: inotify only (fast local disk)","Scan périodique : seul fiable sur NFS distant":"Periodic scan: only reliable option on remote NFS",
  "Pour du local-to-local, rsync suffit et va vite. rclone utile si un jour tu vises un cloud.":"For local-to-local, <b>rsync</b> is enough and fast. rclone helps if you ever target a cloud.",
  "Date : compare taille et horodatage. Checksum : lit le contenu, détecte la corruption, mais relit tout.":"<b>Date</b>: compares size and timestamp. <b>Checksum</b>: reads content, catches corruption, but re-reads everything.",
  "Options avancées (protection & débit)":"Advanced options (protection & bandwidth)",
  "Backup système fidèle 🔒":"Faithful system backup 🔒",
  "Copie exacte : préserve propriétaires, permissions, ACL et hardlinks (flags -aHAX --numeric-ids --fake-super). Les fichiers restent en 1000 sur la destination, mais les vraies identités root sont stockées dans les xattr — ce qui rend le backup restaurable à l'identique (resurrection d'un serveur). Nécessite le conteneur en root (voir compose). Sans cette option, tout est copié en 1000:1000 simple.":"Exact copy: preserves owners, permissions, ACLs and hardlinks (<b>-aHAX --numeric-ids --fake-super</b>). Files stay as 1000 on the destination, but the real root identities are stored in xattrs — making the backup <b>restorable identically</b> (server resurrection). Requires the container as root (see compose). Without this, everything is copied as plain 1000:1000.",
  "Corbeille de sécurité":"Safety trash",
  "Avant d'écraser ou supprimer dans B, déplace l'ancienne version dans un dossier daté .sb-backup/<date>. Filet de sécurité contre les erreurs.":"Before overwriting or deleting in B, moves the old version into a dated <b>.sb-backup/&lt;date&gt;</b> folder. A safety net against mistakes.",
  "Versions à conserver":"Versions to keep",
  "Nombre de dossiers .sb-backup/ gardés. À chaque run, les plus anciens au-delà sont purgés. 0 = tout garder. Ex. 5 = un versioning des 5 dernières exécutions.":"Number of <b>.sb-backup/</b> folders kept. On each run, the oldest beyond that are purged. <b>0</b> = keep all. E.g. <b>5</b> = versioning of the last 5 runs.",
  "Ne pas écraser les fichiers plus récents dans B":"Don't overwrite newer files in B",
  "Si un fichier dans B est plus récent que dans A, on le garde. Utile si plusieurs sources alimentent une même destination.":"If a file in B is newer than in A, it's kept. Useful when several sources feed the same destination.",
  "Limite de débit":"Bandwidth limit",
  "Bride la vitesse pour ne pas saturer le disque/réseau. 10M = 10 Mo/s. Vide = illimité.":"Throttles speed so you don't saturate disk/network. <b>10M</b> = 10 MB/s. Empty = unlimited.",
  "Garde-fou suppressions":"Delete guardrail",
  "En mode miroir, abandonne si plus de N fichiers seraient supprimés. Protège d'un effacement massif accidentel. 0 = désactivé.":"In mirror mode, aborts if more than N files would be deleted. Guards against an accidental mass wipe. 0 = off.",
  "Exclusions":"Exclusions",
  "Motifs à ignorer, séparés par des virgules. Ex. fichiers système NAS (@eaDir), temporaires (*.tmp).":"Patterns to ignore, comma-separated. E.g. NAS system files (<b>@eaDir</b>), temp files (<b>*.tmp</b>).",
  "Annuler":"Cancel","Enregistrer le job":"Save job",
  "Importer depuis le système":"Import from your system",
  "SyncBridge scanne tes scripts et crontabs (montés en lecture seule) pour trouver les commandes rsync et rclone. Chaque copie trouvée peut être importée comme job (créé désactivé — tu vérifies puis actives). L'original n'est pas touché.":"SyncBridge scans your scripts and crontabs (mounted read-only) to find <b>rsync</b> and <b>rclone</b> commands. Each copy found can be imported as a job (created <b>disabled</b> — review, then enable). The original is untouched.",
  "↻ Rescanner":"↻ Rescan","Fermer":"Close","Créer un job":"Create a job",
  "Aucun job pour l'instant":"No jobs yet",
  "Crée ta première tâche : choisis un dossier source, un dossier destination, un comportement, et une fréquence.":"Create your first job: pick a source folder, a destination, a behavior, and a frequency.",
  "accumulation":"accumulate","miroir":"mirror","déplacement":"move","manuel":"manual","constant":"constant","commande":"command","checksum":"checksum","🔒 système":"🔒 system","désactivé":"disabled",
  "Lancer maintenant":"Run now",
  "Simuler (dry-run)":"Simulate (dry-run)","Éditer":"Edit","Cloner":"Clone","Supprimer":"Delete","Arrêter le run en cours":"Stop the running job","Réactiver":"Re-enable","Désactiver":"Disable",
  "Job supprimé":"Job deleted","Échec du clonage":"Clone failed","Job cloné (désactivé par sécurité)":"Job cloned (disabled for safety)","Échec":"Failed","Échec de l'arrêt":"Stop failed",
  "Vérifie les réglages puis enregistre. Le job sera créé désactivé.":"Check the settings then save. The job will be created disabled.",
  "Nom requis":"Name required","Renseigne la commande à exécuter":"Enter the command to run","Source et destination requises":"Source and destination required","Renseigne l'expression cron":"Enter the cron expression","Accès dossier refusé":"Folder access denied","Job désactivé":"Job disabled","Job réactivé":"Job re-enabled",
  "Éditer le job":"Edit job","Importer un job":"Import a job","Importer ce job":"Import this job","pas de planning détecté":"no schedule detected","vide":"empty",
  "Toutes les 15 min":"Every 15 min","Toutes les heures":"Every hour","Toutes les 6 h":"Every 6 h","Chaque jour 3 h":"Every day 3am","Chaque jour minuit":"Every day midnight","Chaque nuit 4 h":"Every night 4am","Lun-Ven 3 h":"Mon–Fri 3am","Dimanche 2 h":"Sunday 2am","1er du mois 5 h":"1st of month 5am"
};
let LANG = localStorage.getItem('sb_lang') || 'en';
const _norm = s => (s||'').replace(/\s+/g,' ').trim();
const _plain = v => v.replace(/<[^>]+>/g,'');
// liste blanche : uniquement des porteurs de texte, jamais des conteneurs (pas de casse de structure)
const I18N_SEL = 'label,.ct,.cd,.hint,option,.tag,.lab,.empty b,.empty p,h2,.section-h,.switch-txt b,.switch-txt span,.cron-live,.tlbl';
function _trEl(el){ const v=I18N_EN[_norm(el.textContent)]; if(v!==undefined) el.innerHTML=v; }
function _trBtn(b){ b.childNodes.forEach(n=>{ if(n.nodeType===3){ const k=_norm(n.nodeValue); if(k){ const v=I18N_EN[k]; if(v) n.nodeValue=' '+_plain(v)+' '; } } }); }
function _trAttrs(el){ ['placeholder','title'].forEach(a=>{ const val=el.getAttribute&&el.getAttribute(a); if(val){ const v=I18N_EN[_norm(val)]; if(v) el.setAttribute(a,_plain(v)); } }); }
function translateTree(root){
  if(!root.querySelectorAll) return;
  root.querySelectorAll('[placeholder],[title]').forEach(_trAttrs);
  root.querySelectorAll(I18N_SEL).forEach(_trEl);
  root.querySelectorAll('button').forEach(_trBtn);
  if(root.matches){ if(root.matches(I18N_SEL)) _trEl(root); if(root.matches('button')) _trBtn(root); _trAttrs(root); }
}
function applyLang(){
  const tog=document.getElementById('langtog'); if(tog) tog.textContent=(LANG==='en')?'🇫🇷 FR':'🇬🇧 EN';
  if(LANG==='en') translateTree(document);
}
function toggleLang(){ LANG=(LANG==='en')?'fr':'en'; localStorage.setItem('sb_lang',LANG); location.reload(); }
new MutationObserver(ms=>{ if(LANG!=='en')return; ms.forEach(m=>m.addedNodes.forEach(n=>{ if(n.nodeType===1) translateTree(n); })); }).observe(document.body,{childList:true,subtree:true});
applyLang();

boot();
