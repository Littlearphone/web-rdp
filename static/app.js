// ── DOM ──
const canvas = document.getElementById('screen'), ctx = canvas.getContext('2d');
let cw=0, ch=0;
const select = document.getElementById('screen-id'), statusEl = document.getElementById('status-text');
const controlCheck = document.getElementById('enable-control'), qualitySlider = document.getElementById('quality');
const qualityVal = document.getElementById('quality-val'), maxwSelect = document.getElementById('maxw');
const statsEl = document.getElementById('stats'), reconnectHint = document.getElementById('reconnect-hint');
const reconnectMsg = document.getElementById('reconnect-msg'), bar = document.getElementById('bar'), view = document.getElementById('view');
const isMobile = /Android|iPhone|iPad|iPod/i.test(navigator.userAgent) && window.innerWidth <= 900;
const useWebCodecs = typeof VideoDecoder !== 'undefined';

let meta = { ox:0, oy:0, pw:1, ph:1, zoom:1.0 }, serverAddr = window.location.host;
let ws=null, reconnectTimer=null, reconnectDelay=5, reconnectCountdown=0, wasConnected=false, lastResKey='';
let currentQ=75, currentMW=0, currentScreen=0, screenCount=1, mobileResOpts=[], mobileUIBuilt=false;
let streamFormat = 'jpeg', useH264Pref = false;

function send(o) { if(ws&&ws.readyState===WebSocket.OPEN) ws.send(JSON.stringify(o)); }
function sendSettings() { send({quality:currentQ, maxw:currentMW, webcodecs:useH264Pref}); }
function sendKey(code,down) { send({key:code, down:down}); }

// ── H.264 WebCodecs 解码器 ──
let h264Decoder=null, h264Ready=false, h264Buf=new Uint8Array(0);

function fSC(d,o) { for(let i=o;i<d.length-3;i++) if(d[i]===0&&d[i+1]===0&&d[i+2]===0&&d[i+3]===1) return i; return -1; }

function bAvcC(sps,pps) {
  let d=new Uint8Array(11+sps.length+pps.length);
  d[0]=1;d[1]=sps[1];d[2]=sps[2];d[3]=sps[3];d[4]=0xFF;d[5]=0xE1;
  d[6]=sps.length>>8;d[7]=sps.length&0xFF;d.set(sps,8);d[8+sps.length]=1;
  let p=9+sps.length;d[p]=pps.length>>8;d[p+1]=pps.length&0xFF;d.set(pps,p+2);return d;
}

function a2Avcc(data) {
  let parts=[], pos=0;
  while(pos<data.length-3){let sc=fSC(data,pos);if(sc<0)break;pos=sc+4;let n=fSC(data,pos);let end=n>=0?n:data.length;let nal=data.slice(pos,end);let h=new Uint8Array(4);h[0]=(nal.length>>24)&0xFF;h[1]=(nal.length>>16)&0xFF;h[2]=(nal.length>>8)&0xFF;h[3]=nal.length&0xFF;parts.push(h);parts.push(nal);pos=end;}
  if(parts.length===0)return data;let t=parts.reduce((s,a)=>s+a.length,0),r=new Uint8Array(t),off=0;for(let a of parts){r.set(a,off);off+=a.length;}return r;
}

function initH264() {
  let pos=0, sps=null, pps=null;
  while(pos<h264Buf.length-3){
    let sc=fSC(h264Buf,pos);if(sc<0)break;
    let n=fSC(h264Buf,sc+4);let end=n>=0?n:h264Buf.length;
    let nal=h264Buf.slice(sc+4,end);let t=nal[0]&0x1F;
    if(t===7)sps=nal; if(t===8)pps=nal;
    if(n<0)break;pos=n+4;
  }
  if(!sps||!pps)return false;
  let desc=bAvcC(sps,pps);
  if(h264Decoder){try{h264Decoder.close()}catch(_){};h264Decoder=null;}
  h264Decoder=new VideoDecoder({output:frame=>{if(cw!==frame.displayWidth||ch!==frame.displayHeight){canvas.width=frame.displayWidth;canvas.height=frame.displayHeight;cw=frame.displayWidth;ch=frame.displayHeight;}ctx.drawImage(frame,0,0);frame.close();},error:e=>{console.error('h264:',e);}});
  h264Decoder.configure({codec:'avc1.42E01E',description:desc});
  h264Ready=true; return true;
}

function decodeH264(data) {
  if(data.length<5||!h264Decoder)return;
  let t=data[4]&0x1F,isKey=(t===5||t===7);
  try{let avcc=a2Avcc(data);h264Decoder.decode(new EncodedVideoChunk({type:isKey?'key':'delta',timestamp:0,data:avcc}));}catch(e){}
}

// ── 分辨率 ──
function buildResolutions(pw,ph) {
  if(isMobile){let m=Math.min(pw,Math.round(innerWidth*(devicePixelRatio||1))),o=[{label:'适配',w:m}];for(let t of[720,480]){if(t>=ph)continue;o.push({label:`${t}p`,w:Math.round(pw*t/ph)});}return o.filter((o,i)=>o.findIndex(x=>x.w===o.w)===i);}
  let o=[{label:`原始 (${pw}×${ph})`,value:0}];for(let t of[1080,720,480]){if(t>=ph)continue;o.push({label:`${Math.round(pw*t/ph)}×${t}`,value:Math.round(pw*t/ph)});}return o;
}
function applyResolutions(pw,ph){let k=`${pw}x${ph}`;if(k===lastResKey)return;lastResKey=k;let o=buildResolutions(pw,ph),c=maxwSelect.value;maxwSelect.innerHTML='';o.forEach(o=>{let e=document.createElement('option');e.value=o.value||o.w;e.textContent=o.label;maxwSelect.appendChild(e);});maxwSelect.value=o.find(o=>String(o.value||o.w)===c)?c:String(o[0].value||o[0].w);if(isMobile)sendSettings();}

// ── 连接 ──
function connect(){if(ws){ws.onclose=null;ws.close();ws=null;}h264Buf=new Uint8Array(0);h264Ready=false;if(h264Decoder){try{h264Decoder.close()}catch(_){};h264Decoder=null;}clearReconnectTimer();reconnectHint.style.display='none';lastResKey='';statusEl.textContent='连接中...';statusEl.style.color='#f1c40f';ws=new WebSocket(`ws://${serverAddr}/ws`);ws.binaryType='arraybuffer';ws.onopen=()=>{wasConnected=true;statusEl.textContent='已连接';statusEl.style.color='#27ae60';reconnectDelay=5;sendSettings();};ws.onmessage=onMessage;ws.onclose=()=>{statusEl.textContent='连接断开';statusEl.style.color='#e74c3c';statsEl.innerHTML='离线';if(h264Decoder){try{h264Decoder.close()}catch(_){};h264Decoder=null;h264Ready=false;}h264Buf=new Uint8Array(0);if(wasConnected)startReconnect();};ws.onerror=()=>{if(!wasConnected){statusEl.textContent='连接失败';statusEl.style.color='#e74c3c';}};}

function onMessage(event){
  if(typeof event.data==='string'){try{let s=JSON.parse(event.data);
    if(s.user){statsEl.setAttribute('data-user',s.user);if(s.format)streamFormat=s.format;return;}
    if(isMobile){let u=statsEl.getAttribute('data-user')||'',p=matchMedia('(orientation:portrait)').matches;statsEl.innerHTML=p?`${u}&emsp;${s.fps}fps&emsp;${s.kb}KB`:`${u}<br>${s.fps}fps<br>${s.kb}KB`;}else{statsEl.innerHTML=`${s.w}×${s.h} Q${s.q} │ ${s.fps}fps │ ${s.enc_ms}ms │ ${s.kb}KB/f │ ${(s.kb*s.fps/1024).toFixed(1)}MB/s`;}
    if(s.owner!==undefined){let me=statsEl.getAttribute('data-user')||'',im=s.owner===me;controlCheck.disabled=!im&&s.owner!=='';controlCheck.checked=im;controlCheck.parentElement.title=s.owner?`控制权:${s.owner}`:'点击获取控制权';canvas.style.cursor='crosshair';}
    if(s.screens>0&&s.screens!==screenCount){screenCount=s.screens;isMobile?(mobileUIBuilt=false,updateMobileUI()):updateDesktopScreens();}
  }catch(_){}return;}

  let buf=event.data;
  if(streamFormat==='h264'&&useWebCodecs){
    let chunk=new Uint8Array(buf);
    let t2=new Uint8Array(h264Buf.length+chunk.length);t2.set(h264Buf);t2.set(chunk,h264Buf.length);h264Buf=t2;
    if(!h264Ready){if(!initH264())return;}
    // 处理缓冲中的 NAL 组
    while(h264Ready&&h264Buf.length>4){
      let sc=fSC(h264Buf,4);if(sc<0)break;
      let c=h264Buf.slice(0,sc);h264Buf=h264Buf.slice(sc);
      if(c.length>4)decodeH264(c);
    }
  }else{
    let dv=new DataView(buf);meta={ox:dv.getInt32(0,true),oy:dv.getInt32(4,true),pw:dv.getInt32(8,true),ph:dv.getInt32(12,true),zoom:dv.getFloat64(16,true)};
    if(isMobile){let k=`${meta.pw}x${meta.ph}`;if(k!==lastResKey){lastResKey=k;mobileUIBuilt=false;updateMobileResolutions(meta.pw,meta.ph);}}else{let k=`${meta.pw}x${meta.ph}`;if(k!==lastResKey){lastResKey=k;buildDesktopResolutions(meta.pw,meta.ph);}}
    let jpg=new Uint8Array(buf,24);createImageBitmap(new Blob([jpg],{type:'image/jpeg'})).then(bmp=>{if(cw!==meta.pw||ch!==meta.ph){canvas.width=meta.pw;canvas.height=meta.ph;cw=meta.pw;ch=meta.ph;}ctx.drawImage(bmp,0,0);bmp.close();}).catch(()=>{});
  }
}
connect();

// ── 重连 ──
function startReconnect(){scheduleReconnect();}
function scheduleReconnect(){clearReconnectTimer();reconnectCountdown=reconnectDelay;updateReconnectUI();reconnectHint.style.display='flex';reconnectTimer=setInterval(()=>{reconnectCountdown--;if(reconnectCountdown<=0){clearReconnectTimer();reconnectHint.style.display='none';reconnectDelay=Math.min(reconnectDelay*2,30);connect();return;}updateReconnectUI();},1000);}
function updateReconnectUI(){reconnectMsg.textContent=`${reconnectCountdown} 秒后自动重连...`;}
function manualReconnect(){clearReconnectTimer();reconnectHint.style.display='none';connect();}
function clearReconnectTimer(){if(reconnectTimer){clearInterval(reconnectTimer);reconnectTimer=null;}}
statusEl.onclick=()=>{if(!ws||ws.readyState!==WebSocket.OPEN){clearReconnectTimer();reconnectHint.style.display='none';connect();}};

// ── 桌面控制 ──
if(!isMobile){
  function updateDesktopScreens(){let c=select.value;select.innerHTML='';for(let i=0;i<screenCount;i++){let e=document.createElement('option');e.value=i;e.textContent=i===0?'主屏 (0)':`副屏 (${i})`;select.appendChild(e);}select.value=c<screenCount?c:'0';}
  function buildDesktopResolutions(pw,ph){let o=buildResolutions(pw,ph),c=maxwSelect.value;maxwSelect.innerHTML='';o.forEach(o=>{let e=document.createElement('option');e.value=o.value;e.textContent=o.label;maxwSelect.appendChild(e);});maxwSelect.value=o.find(o=>String(o.value)===c)?c:'0';}
  qualitySlider.oninput=()=>{qualityVal.textContent=qualitySlider.value;};qualitySlider.onchange=()=>{currentQ=parseInt(qualitySlider.value);sendSettings();};maxwSelect.onchange=()=>{currentMW=parseInt(maxwSelect.value);sendSettings();};select.onchange=()=>{currentScreen=parseInt(select.value);send({screen:currentScreen});lastResKey='';};
  let h264Toggle=document.getElementById('use-h264');if(h264Toggle){h264Toggle.checked=useH264Pref;h264Toggle.onchange=()=>{useH264Pref=h264Toggle.checked;sendSettings();setTimeout(connect,100);};}
  controlCheck.onchange=()=>{send({control:controlCheck.checked});canvas.style.cursor='crosshair';};
  let active=false,dragStart=null,dragging=false,lastMoveSent=0;
  canvas.onmousedown=e=>{if(!controlCheck.checked||e.target!==canvas)return;e.preventDefault();if(e.button===0){active=true;let c=screenCoords(e);if(c.fx==null)return;dragStart=c;dragging=false;}};
  canvas.onmousemove=e=>{if(!controlCheck.checked)return;if(!active||!dragStart){let n=Date.now();if(n-lastMoveSent<30)return;lastMoveSent=n;let c=screenCoords(e);if(c.fx!=null)send({mx:c.fx,my:c.fy});return;}let c=screenCoords(e);if(c.fx==null)return;if(Math.abs(c.fx-dragStart.fx)>3||Math.abs(c.fy-dragStart.fy)>3)dragging=true;};
  canvas.onmouseup=e=>{if(!active)return;active=false;e.preventDefault();let c=screenCoords(e);if(c.fx==null)return;if(dragging){send({dx1:dragStart.fx,dy1:dragStart.fy,dx2:c.fx,dy2:c.fy});}else{fetch(`http://${serverAddr}/click?x=${c.fx}&y=${c.fy}`).catch(()=>{});}dragging=false;dragStart=null;};
  view.addEventListener('contextmenu',e=>{if(!controlCheck.checked||e.target!==canvas)return;e.preventDefault();let c=screenCoords(e);if(c.fx==null)return;send({rx:c.fx,ry:c.fy});});
  let pc=new Set(['F1','F3','F5','F11','F12','Tab','Escape','AltLeft','AltRight','ControlLeft','ControlRight','MetaLeft','MetaRight']);
  window.addEventListener('keydown',e=>{if(!controlCheck.checked)return;if(pc.has(e.code)||e.ctrlKey||e.altKey||e.metaKey){e.preventDefault();e.stopPropagation();}sendKey(e.code,true);},{capture:true});
  window.addEventListener('keyup',e=>{if(!controlCheck.checked)return;e.preventDefault();e.stopPropagation();sendKey(e.code,false);},{capture:true});
}

// ── 手机 ──
if(isMobile){
  function updateMobileResolutions(pw,ph){mobileResOpts=buildResolutions(pw,ph);if(!currentMW||!mobileResOpts.find(o=>o.w===currentMW))currentMW=mobileResOpts[0].w;mobileUIBuilt=false;updateMobileUI();}
  function updateMobileUI(){if(mobileUIBuilt){bar.querySelectorAll('.ctrl-btn').forEach(b=>b.classList.remove('active'));bar.querySelector(`[data-q="${currentQ}"]`)?.classList.add('active');bar.querySelector(`[data-mw="${currentMW}"]`)?.classList.add('active');let sb=bar.querySelector('.scr-btn');if(sb)sb.textContent=screenCount>1?`屏${currentScreen}`:'主屏';return;}mobileUIBuilt=true;bar.querySelectorAll('.ctrl-rows,.ctrl-row,.ctrl-btn,.ctrl-group').forEach(e=>e.remove());let rows=bar.querySelector('.ctrl-rows');if(!rows){rows=document.createElement('div');rows.className='ctrl-rows';bar.appendChild(rows);}let row=()=>{let d=document.createElement('div');d.className='ctrl-row';rows.appendChild(d);return d;};let addBtn=(r,t,cl,click)=>{let b=document.createElement('span');b.className='ctrl-btn '+cl;b.textContent=t;b.onclick=click;r.appendChild(b);};let addGroup=(r,btns)=>{let g=document.createElement('span');g.className='ctrl-group';btns.forEach(b=>{let e=document.createElement('span');e.className='ctrl-btn';if(b.active)e.classList.add('active');e.textContent=b.label;e.onclick=b.click;if(b.data)Object.entries(b.data).forEach(([k,v])=>e.setAttribute('data-'+k,v));g.appendChild(e);});r.appendChild(g);};let r=row();addBtn(r,screenCount>1?`屏${currentScreen}`:'主屏','scr-btn',()=>{if(screenCount>1){currentScreen=(currentScreen+1)%screenCount;send({screen:currentScreen});mobileUIBuilt=false;updateMobileUI();}});addGroup(r,[{label:'低',active:currentQ===40,data:{q:40},click:()=>{currentQ=40;sendSettings();updateMobileUI();}},{label:'中',active:currentQ===60,data:{q:60},click:()=>{currentQ=60;sendSettings();updateMobileUI();}},{label:'高',active:currentQ===80,data:{q:80},click:()=>{currentQ=80;sendSettings();updateMobileUI();}}]);addGroup(r,mobileResOpts.map(o=>({label:o.label,active:currentMW===o.w,data:{mw:o.w},click:()=>{currentMW=o.w;sendSettings();updateMobileUI();}})));}
}

// ── 坐标 ──
function eventPos(e){let t=(e.touches&&e.touches[0])||(e.changedTouches&&e.changedTouches[0]);if(t)return{clientX:t.clientX,clientY:t.clientY,target:e.target};return{clientX:e.clientX,clientY:e.clientY,target:e.target};}
function screenCoords(e){let p=eventPos(e),r=canvas.getBoundingClientRect(),ra=meta.pw/meta.ph,cr=r.width/r.height,aw,ah,ox=0,oy=0;if(ra>cr){aw=r.width;ah=r.width/ra;oy=(r.height-ah)/2;}else{ah=r.height;aw=r.height*ra;ox=(r.width-aw)/2;}let cx=(p.clientX-r.left-ox)/aw,cy=(p.clientY-r.top-oy)/ah;if(cx<0||cx>1||cy<0||cy>1)return{};return{fx:Math.round((meta.ox*meta.zoom)+(cx*meta.pw*meta.zoom)),fy:Math.round((meta.oy*meta.zoom)+(cy*meta.ph*meta.zoom))};}
