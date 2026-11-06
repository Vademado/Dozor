import './player.js';
import {CameraConsole} from './console.js';

const $ = id => document.getElementById(id);
const cameraConsole = new CameraConsole();
const state = {
  config: null, status: null, player: null, recent: [], journal: [],
  page: 'live', formDirty: false, sourceDirty: false, editingROI: false,
  draftROI: null, drawing: null, playing: false, metrics: null,
  sessionStarted: 0, retriesBeforePlayer: 0, retries: 0,
  toastTimer: 0, lastPlayerLog: 0, events: null,
};
const sources = {system: 'Система', operator: 'Оператор', stream: 'RTSP-клиент', browser: 'Плеер', detector: 'Детектор'};
const levels = {info: 'Информация', warning: 'Внимание', error: 'Ошибка'};
const formatTime = value => new Date(value).toLocaleTimeString('ru-RU', {hour:'2-digit', minute:'2-digit', second:'2-digit'});
const text = (id, value) => { $(id).textContent = value; };
const duration = seconds => `${Math.floor(seconds / 3600).toString().padStart(2,'0')}:${Math.floor(seconds / 60 % 60).toString().padStart(2,'0')}:${Math.floor(seconds % 60).toString().padStart(2,'0')}`;

async function api(path, options = {}) {
  const response = await fetch(path, {...options, headers: {'Content-Type':'application/json', ...options.headers}, signal: options.signal ?? AbortSignal.timeout(15000)});
  if (!response.ok) {
    const error = await response.json().catch(() => ({error:`Ошибка HTTP ${response.status}`}));
    throw new Error(error.error || `Ошибка HTTP ${response.status}`);
  }
  return response.status === 204 ? null : response.json();
}

function toast(message, error = false) {
  clearTimeout(state.toastTimer);
  const node = $('toast'); node.textContent = message;
  node.classList.toggle('error', error); node.hidden = false;
  state.toastTimer = setTimeout(() => node.hidden = true, error ? 9000 : 4500);
}

async function action(button, callback) {
  button.disabled = true;
  try { await callback(); } catch (error) { toast(error.message, true); }
  finally { button.disabled = false; updateButtons(); }
}

function setConfig(config, forceForm = false) {
  const sourceChanged = state.config && state.config.source !== config.source;
  state.config = config;
  if (!state.sourceDirty) $('sourceInput').value = config.source;
  $('roiEnabled').checked = config.roi.enabled;
  text('cameraTitle', config.camera_name);
  try { document.querySelector('.system-address').firstChild.textContent = `${new URL(config.source).hostname} `; } catch { /* validated on server */ }
  if (!state.formDirty || forceForm) fillSettings();
  drawROI(); updateROIMini(); updateButtons();
  if (sourceChanged) {
    unmountPlayer();
  }
}

function updateButtons() {
  const preview = state.config?.preview_enabled;
  $('startButton').lastChild.textContent = state.sourceDirty ? 'Применить и запустить' : 'Запустить';
  $('stopButton').disabled = !preview;
  $('snapshotButton').disabled = !state.playing;
  $('reconnectButton').disabled = !preview;
  $('editROIButton').disabled = !state.playing || state.editingROI;
}

function applyStatus(status) {
  state.status = status;
  const relay=status.stream.backend==='go2rtc';
  const canView=status.stream.online || (relay&&status.stream.available);
  text('navEventCount', status.journal_size > 999 ? '999+' : status.journal_size);
  $('sidebarBridgeDot').className = `status-dot ${status.stream.online ? 'green' : 'warn'}`;
  text('sidebarBridgeLabel', status.stream.online ? 'Камера подключена через go2rtc' : relay&&status.stream.available?'go2rtc готов к просмотру':'go2rtc недоступен');
  text('bridgeStatus', status.stream.online ? (status.stream.error ? 'Ошибка потока' : 'Подключён') : 'Нет соединения');
  const kbps = status.stream.bitrate_kbps;
  metric('metricBitrate', kbps == null ? '—' : Math.round(kbps).toLocaleString('ru-RU'), 'Кбит/с');
  if (!state.playing) text('codecValue', extractCodec((status.stream.codecs || []).join(' ')) || '—');
  text('inputFPS', relay ? 'Нет данных API' : status.stream.input_fps == null ? '—' : `${status.stream.input_fps.toFixed(1)} FPS`);
  text('rtpMissing', relay ? 'Нет данных о потерях' : `${status.stream.rtp_missing ?? 0} / ${status.stream.rtp_packets ?? 0}`);
  text('queueResets', relay ? 'Управляет go2rtc' : status.stream.queue_resets ?? 0);
  text('metricViewers', status.stream.viewers ?? '—');
  const agent = status.agent_online;
  const applied = status.policy_applied;
  $('agentDot').className = `status-dot ${agent && applied ? 'green' : 'warn'}`;
  text('agentNotice', !agent ? 'Агент детекции не подключён' : applied ? 'Политика подтверждена агентом' : 'Агент ещё не применил политику');
  text('protectionStatus', status.armed ? (applied ? 'Охрана включена' : 'Ожидаем подтверждение') : (agent && !applied ? 'Ожидаем снятие с охраны' : 'Режим наблюдения'));
  text('protectionDescription', status.armed
    ? (applied ? 'Агент подтвердил режим охраны и текущую версию настроек.' : 'Режим сохранён в панели. Охрана будет подтверждена после ответа агента.')
    : 'Просмотр камеры доступен независимо от режима охраны.');
  $('armButton').querySelector('span').textContent = status.armed ? 'Снять с охраны' : 'Включить охрану';
  const telemetry = agent ? status.telemetry : null;
  text('detectorModel', telemetry?.model || 'Нет телеметрии');
  text('personCount', telemetry ? telemetry.persons : '—');
  text('vehicleCount', telemetry ? telemetry.vehicles : '—');
  text('inferenceTime', telemetry ? telemetry.inference_ms.toFixed(1) : '—');
  text('detectorNote', telemetry ? `Последний ответ: ${formatTime(telemetry.received_at)} · ${telemetry.fps.toFixed(1)} FPS инференса` : 'Показатели появятся после подключения агента.');
  const enabled = status.preview_enabled;
  if (!enabled) {
    unmountPlayer();
    placeholder('Просмотр остановлен', 'Камера и агент охраны продолжают работать независимо от просмотра.');
    setPlaybackState('stopped');
  } else if (status.stream.backend !== 'go2rtc' && (!canView || !status.stream.registered || status.stream.error)) {
    unmountPlayer();
    placeholder('Нет соединения с камерой', status.stream.error || 'Go-сервер подключается к RTSP-камере. Повтор выполняется автоматически.');
    setPlaybackState('offline');
  } else if (!state.player && state.page === 'live') {
    mountPlayer();
  } else if (!relay && state.player && state.player.codec !== extractCodec((status.stream.codecs || []).join(' '))) {
    unmountPlayer(); mountPlayer();
  }
  // Refresh settings after changes in another tab, preserving an unsaved form.
  if (state.config && status.policy_revision !== state.config.revision) {
    api('/api/settings').then(config => {
      setConfig(config);
      if (state.status) applyStatus(state.status);
    }).catch(() => {});
  }
}

function extractCodec(value) {
  if (/avc1|h264|h\.264/i.test(value)) return 'H.264';
  if (/hvc1|hev1|h265|h\.265/i.test(value)) return 'H.265';
  if (/mjpeg|jpeg/i.test(value)) return 'MJPEG';
  return '';
}

function placeholder(title, description) {
  $('videoPlaceholder').hidden = false;
  text('placeholderTitle', title); text('placeholderText', description);
}

function setPlaybackState(value, message = '') {
  state.playing = value === 'playing';
  const names = {playing:'Видео поступает', connecting:'Подключение', reconnecting:'Переподключение', offline:'Нет RTSP', stopped:'Просмотр остановлен', unsupported:'Кодек недоступен', stalled:'Кадры не поступают', embedded:'Плеер go2rtc открыт'};
  const label = names[value] || 'Ожидание видео';
  text('headingStatus', label); text('playbackStatus', label);
  $('headingDot').className = `status-dot ${state.playing ? 'green' : 'warn'}`;
  $('liveBadge').classList.toggle('playing', state.playing);
  $('liveBadge').lastChild.textContent = state.playing ? 'LIVE' : value === 'stopped' ? 'ПАУЗА' : 'ОЖИДАНИЕ';
  $('videoConnection').classList.toggle('playing', state.playing);
  if (state.playing || value === 'embedded') {
    $('videoPlaceholder').hidden = true;
    text('sourceFoot', value === 'embedded' ? 'Просмотр потока luckfox в штатном плеере go2rtc' : 'Изображение поступает с камеры');
  } else {
    text('sourceFoot', value === 'stopped' ? 'Просмотр остановлен оператором' : value === 'unsupported' ? 'Проверьте поддержку кодека браузером' : 'Ожидаем видеоданные');
    if (value === 'connecting') placeholder('Подключаемся к камере', message || 'Ожидаем первый кадр. Это может занять несколько секунд.');
    if (value === 'reconnecting') placeholder('Восстанавливаем трансляцию', message);
    if (value === 'unsupported') placeholder('Браузер не поддерживает кодек камеры', message);
    text('videoInfo', label);
  }
  updateButtons();
}

function mountPlayer() {
  if (state.player || !state.status?.preview_enabled) return;
  const player = document.createElement('guard-player');
  state.player = player;
  if (!state.sessionStarted) state.sessionStarted = Date.now();
  player.addEventListener('player-state', event => {
    if (state.player === player) setPlaybackState(event.detail.state);
  });
  $('playerMount').appendChild(player);
  text('transportValue', 'Авто · штатный go2rtc');
  player.start();
}
function unmountPlayer() {
  if (!state.player) return;
  state.player.dispose(); state.player.remove(); state.player = null;
  state.playing = false; state.metrics = null;
  text('metricResolution','—'); metric('metricFPS','—','FPS'); metric('metricDropped','—','кадров');
  text('frameAge','—'); text('videoBuffer','—');
  text('browserPacketLoss','—');text('decodeTime','—');
}

function metric(id, value, unit) {
  const node = $(id); node.replaceChildren(document.createTextNode(`${value} `));
  const em = document.createElement('em'); em.textContent = unit; node.appendChild(em);
}

async function preview(enabled) {
  const body = {enabled};
  if (enabled) body.source = $('sourceInput').value.trim();
  const result = await api('/api/preview', {method:'POST',body:JSON.stringify(body)});
  state.sourceDirty = false;
  unmountPlayer(); setConfig(result.settings); applyStatus(result.status);
  if (!enabled) { state.sessionStarted = 0; toast('Просмотр остановлен. Режим охраны не изменён.'); }
  else toast('Источник сохранён и применён. Открыт плеер go2rtc.');
}

async function reconnect() {
  state.retries++; text('reconnectCount',`Переподключений: ${state.retries}`);
  unmountPlayer();
  await api('/api/player/events',{method:'POST',body:JSON.stringify({type:'player_reconnected'})});
  await preview(true);
}

async function snapshot() {
  const video = state.player?.video;
  if (!video || !video.videoWidth || !state.playing) throw new Error('Сначала дождитесь изображения с камеры.');
  const canvas = document.createElement('canvas'); canvas.width = video.videoWidth; canvas.height = video.videoHeight;
  canvas.getContext('2d').drawImage(video,0,0);
  const blob = await new Promise(resolve => canvas.toBlob(resolve,'image/png'));
  if (!blob) throw new Error('Не удалось сохранить кадр.');
  download(blob,`dozor-${new Date().toISOString().replaceAll(':','-')}.png`);
  api('/api/player/events',{method:'POST',body:JSON.stringify({type:'snapshot_created'})}).catch(() => {});
  toast('Снимок кадра сохранён в загрузки браузера.');
}

function download(blob, name) {
  const url = URL.createObjectURL(blob), link = document.createElement('a');
  link.href = url; link.download = name; link.click();
  setTimeout(() => URL.revokeObjectURL(url),1000);
}

function fillSettings() {
  if (!state.config) return;
  const form = $('settingsForm').elements, cfg = state.config;
  for (const name of ['camera_name','source','confidence','dwell_seconds','cooldown_seconds']) form[name].value = cfg[name];
  form.preview_enabled.checked = cfg.preview_enabled;
  text('confidenceOutput',`${Math.round(cfg.confidence*100)}%`);
  text('settingsRevision',`Версия политики ${cfg.revision}`);
  state.formDirty = false;
}

async function saveSettings(event) {
  event.preventDefault();
  await action(event.submitter,async () => {
    const values = new FormData($('settingsForm'));
    const cfg = {...state.config,
      camera_name: values.get('camera_name').trim(), source: values.get('source').trim(),
      confidence: Number(values.get('confidence')), dwell_seconds: Number(values.get('dwell_seconds')),
      cooldown_seconds: Number(values.get('cooldown_seconds')), preview_enabled: values.has('preview_enabled'),
    };
    const saved = await api('/api/settings',{method:'PUT',body:JSON.stringify(cfg)});
    state.formDirty = false; state.sourceDirty = false;
    setConfig(saved,true);
    unmountPlayer(); applyStatus(await api('/api/status'));
    toast('Настройки сохранены. Агент получит новую версию политики.');
  });
}

function videoRect() {
  const canvas = $('roiCanvas'), width = canvas.clientWidth, height = canvas.clientHeight;
  const video = state.player?.video;
  const ratio = video?.videoWidth && video?.videoHeight ? video.videoWidth/video.videoHeight : 16/9;
  const w = Math.min(width,height*ratio), h = w/ratio;
  return {x:(width-w)/2,y:(height-h)/2,width:w,height:h};
}

function drawROI() {
  const canvas = $('roiCanvas'); if (!canvas.clientWidth || !state.config) return;
  const scale = window.devicePixelRatio || 1;
  canvas.width = Math.round(canvas.clientWidth*scale); canvas.height = Math.round(canvas.clientHeight*scale);
  const ctx = canvas.getContext('2d'); ctx.scale(scale,scale);
  const roi = state.editingROI ? state.draftROI : state.config.roi;
  if (!roi || !roi.enabled && !state.editingROI) return;
  const rect = videoRect();
  const x=rect.x+roi.x*rect.width, y=rect.y+roi.y*rect.height, w=roi.width*rect.width, h=roi.height*rect.height;
  ctx.strokeStyle='#d2b76b'; ctx.fillStyle='#d2b76b12'; ctx.lineWidth=1.4;
  ctx.fillRect(x,y,w,h);ctx.strokeRect(x,y,w,h);
  const length=9;ctx.lineWidth=2.5;
  for (const [cx,cy,sx,sy] of [[x,y,1,1],[x+w,y,-1,1],[x,y+h,1,-1],[x+w,y+h,-1,-1]]) {
    ctx.beginPath();ctx.moveTo(cx,cy+sy*length);ctx.lineTo(cx,cy);ctx.lineTo(cx+sx*length,cy);ctx.stroke();
  }
  ctx.font='10px Segoe UI';ctx.fillStyle='#e4cf91';ctx.fillText('ЗОНА НАБЛЮДЕНИЯ',x+7,Math.max(14,y-8));
}

function updateROIMini() {
  const roi = state.draftROI || state.config?.roi; if (!roi) return;
  const node = $('roiMiniRect');
  Object.assign(node.style,{left:`${roi.x*100}%`,top:`${roi.y*100}%`,width:`${roi.width*100}%`,height:`${roi.height*100}%`,opacity:roi.enabled || state.editingROI ? '1' : '.35'});
}

function editROI(enabled) {
  state.editingROI = enabled; state.drawing = null;
  state.draftROI = enabled ? {...state.config.roi,enabled:true} : null;
  $('roiCanvas').classList.toggle('editing',enabled); $('roiHint').hidden = !enabled;
  $('roiActions').hidden = !enabled; $('editROIButton').hidden = enabled;
  updateButtons(); drawROI(); updateROIMini();
}

function roiPoint(event) {
  const bounds = $('roiCanvas').getBoundingClientRect(), rect = videoRect();
  return {x:Math.max(0,Math.min(1,(event.clientX-bounds.left-rect.x)/rect.width)),y:Math.max(0,Math.min(1,(event.clientY-bounds.top-rect.y)/rect.height))};
}

function drawPointer(event) {
  if (!state.drawing) return;
  const p = roiPoint(event), start = state.drawing;
  state.draftROI = {enabled:true,x:Math.min(start.x,p.x),y:Math.min(start.y,p.y),width:Math.abs(start.x-p.x),height:Math.abs(start.y-p.y)};
  drawROI(); updateROIMini();
}

async function saveROI() {
  const roi = state.draftROI || {...state.config.roi,enabled:$('roiEnabled').checked};
  const cfg = await api('/api/settings',{method:'PUT',body:JSON.stringify({...state.config,roi})});
  setConfig(cfg);editROI(false);toast('Зона сохранена. Границы передаются агенту в долях кадра.');
}

function makeEventNode(event, compact = false) {
  if (compact) {
    const row = document.createElement('div');row.className='recent-row';
    const time=document.createElement('time');time.textContent=formatTime(event.time);
    const dot=document.createElement('span');dot.className=`status-dot ${event.level==='error'?'red':event.level==='warning'?'warn':'green'}`;
    const message=document.createElement('p');message.textContent=event.message;message.title=event.message;
    const source=document.createElement('small');source.textContent=sources[event.source] || event.source;
    row.append(time,dot,message,source);return row;
  }
  const row=document.createElement('tr');
  const date=new Date(event.time);
  const values=[`${date.toLocaleDateString('ru-RU')} ${formatTime(event.time)}`,levels[event.level] || event.level,sources[event.source] || event.source,event.type,event.message];
  if (event.track_id!=null) values[4]+=` · track ID ${event.track_id}`;
  if (event.confidence!=null) values[4]+=` · ${Math.round(event.confidence*100)}%`;
  values.forEach((value,index)=>{
    const cell=document.createElement('td');
    if(index===1){const badge=document.createElement('span');badge.className=`level-badge ${event.level}`;badge.textContent=value;cell.appendChild(badge);}
    else cell.textContent=value;
    row.appendChild(cell);
  });return row;
}

function renderRecent() {
  const container=$('recentEvents');container.replaceChildren(...state.recent.slice(0,4).map(event=>makeEventNode(event,true)));
  if(!state.recent.length){const empty=document.createElement('p');empty.className='empty-note';empty.textContent='Событий пока нет.';container.appendChild(empty);}
}

function filterQuery() {return new URLSearchParams({limit:'500',q:$('logSearch').value,level:$('logLevel').value,source:$('logSource').value});}

function renderJournal() {
  $('eventsTable').replaceChildren(...state.journal.map(event=>makeEventNode(event)));
  $('eventsEmpty').hidden=state.journal.length>0;
  text('eventsCount',`Показано событий: ${state.journal.length}`);
}

let journalRequest=0;
async function loadJournal() {
  const request=++journalRequest;
  const result=await api(`/api/events?${filterQuery()}`);
  if(request!==journalRequest)return;
  state.journal=result.items;renderJournal();
}

function acceptEvent(event) {
  if(state.recent.some(item=>item.id===event.id))return;
  state.recent.unshift(event);state.recent=state.recent.slice(0,100);renderRecent();
  if(state.page==='events') loadJournal().catch(()=>{});
}

function serverConnection(online) {
  $('serverDot').className=`status-dot ${online?'green':'warn'}`;
  text('serverLabel',online?'Сервер на связи':'Нет связи с сервером');
  $('connectionBanner').hidden=online;
}

function connectEvents() {
  const latest=state.recent[0]?.id || 0;
  const events=new EventSource(`/api/events/stream?after=${latest}`);state.events=events;
  events.addEventListener('status',event=>{serverConnection(true);applyStatus(JSON.parse(event.data));});
  events.addEventListener('journal',event=>acceptEvent(JSON.parse(event.data)));
  events.onerror=()=>serverConnection(false);
}

function navigate() {
  let page=location.hash.slice(1) || 'live';
  if(!['live','events','settings','console','help'].includes(page))page='live';
  state.page=page;
  document.querySelectorAll('.page').forEach(node=>node.classList.toggle('active',node.id===`page-${page}`));
  document.querySelectorAll('[data-page]').forEach(node=>{node.classList.toggle('active',node.dataset.page===page);if(node.dataset.page===page)node.setAttribute('aria-current','page');else node.removeAttribute('aria-current');});
  text('breadcrumb',{live:'Прямая трансляция',events:'Журнал событий',settings:'Настройки системы',console:'Консоль камеры',help:'Как это работает'}[page]);
  cameraConsole.setVisible(page === 'console');
  if(page!=='live') {unmountPlayer();if(state.editingROI)editROI(false);}
  else if(state.status)applyStatus(state.status);
  if(page==='events')loadJournal().catch(error=>toast(error.message,true));
  if(page==='settings'&&!state.formDirty)fillSettings();
  window.scrollTo({top:0,behavior:'instant'});
}

function bindControls() {
	$('sourceInput').readOnly = false;
  $('sourceInput').addEventListener('input',()=>{state.sourceDirty=true;updateButtons();});
  $('sourceInput').addEventListener('keydown',event=>{
    if(event.key==='Enter') {event.preventDefault();$('startButton').click();}
  });
  $('startButton').addEventListener('click',()=>action($('startButton'),()=>preview(true)));
  $('stopButton').addEventListener('click',()=>action($('stopButton'),()=>preview(false)));
  for(const id of ['reconnectButton','placeholderRetry'])$(id).addEventListener('click',()=>action($(id),reconnect));
  $('snapshotButton').addEventListener('click',()=>action($('snapshotButton'),snapshot));
  $('muteButton').addEventListener('click',()=>{
    const video=state.player?.video;if(!video)return;
    video.muted=!video.muted;
    $('muteButton').querySelector('use').setAttribute('href',video.muted?'#i-mute':'#i-volume');
    $('muteButton').title=video.muted?'Включить звук':'Выключить звук';
    $('muteButton').setAttribute('aria-label',$('muteButton').title);
  });
  $('fullscreenButton').addEventListener('click',()=>{
    const promise=document.fullscreenElement?document.exitFullscreen():$('videoScreen').requestFullscreen();
    promise.catch(()=>toast('Браузер не разрешил полноэкранный режим.',true));
  });
  $('armButton').addEventListener('click',()=>action($('armButton'),async()=>{
    const cfg=await api('/api/arm',{method:'POST',body:JSON.stringify({armed:!state.status?.armed})});
    setConfig(cfg);applyStatus(await api('/api/status'));
    toast('Запрошенный режим сохранён. Ожидаем подтверждение агента.');
  }));
  $('settingsForm').addEventListener('submit',saveSettings);
  $('settingsForm').addEventListener('input',()=>{state.formDirty=true;text('confidenceOutput',`${Math.round(Number($('settingsForm').elements.confidence.value)*100)}%`);});
  $('editROIButton').addEventListener('click',()=>editROI(true));
  $('cancelROIButton').addEventListener('click',()=>editROI(false));
  $('saveROIButton').addEventListener('click',()=>action($('saveROIButton'),saveROI));
  $('roiEnabled').addEventListener('change',async()=>{
    try{await saveROI();}catch(error){$('roiEnabled').checked=state.config.roi.enabled;toast(error.message,true);}
  });
  $('roiCanvas').addEventListener('pointerdown',event=>{
    if(!state.editingROI)return;
    state.drawing=roiPoint(event);$('roiCanvas').setPointerCapture(event.pointerId);drawPointer(event);
  });
  $('roiCanvas').addEventListener('pointermove',drawPointer);
  $('roiCanvas').addEventListener('pointerup',event=>{drawPointer(event);state.drawing=null;});
  $('roiCanvas').addEventListener('pointercancel',()=>state.drawing=null);
  let searchTimer;
  $('logSearch').addEventListener('input',()=>{clearTimeout(searchTimer);searchTimer=setTimeout(()=>loadJournal().catch(error=>toast(error.message,true)),250);});
  for(const id of ['logLevel','logSource'])$(id).addEventListener('change',()=>loadJournal().catch(error=>toast(error.message,true)));
  $('exportButton').addEventListener('click',()=>action($('exportButton'),async()=>{
    const response=await fetch(`/api/events/export?${filterQuery()}`);
    if(!response.ok)throw new Error('Не удалось экспортировать журнал.');
    download(await response.blob(),`dozor-events-${new Date().toISOString().slice(0,10)}.jsonl`);
  }));
  window.addEventListener('hashchange',navigate);
  window.addEventListener('resize',drawROI);
  document.addEventListener('fullscreenchange',drawROI);
  window.addEventListener('beforeunload',()=>{state.events?.close();unmountPlayer();});
}

function updateClocks() {
  const now=new Date();
  text('headerDate',now.toLocaleDateString('ru-RU',{day:'2-digit',month:'long',year:'numeric'}));
  text('videoClock',`${now.toLocaleDateString('ru-RU')}  ${formatTime(now)}`);
  text('sessionUptime',state.sessionStarted?duration((Date.now()-state.sessionStarted)/1000):'—');
}

async function initialize() {
  try {
    const capabilities = window.RTCRtpReceiver?.getCapabilities?.('video');
    const codecs = [...new Set((capabilities?.codecs || []).map(codec => extractCodec(codec.mimeType)).filter(Boolean))];
    text('browserCodecs', codecs.length ? codecs.join(' · ') : 'H.264/H.265 недоступны');
  } catch { text('browserCodecs', 'Не удалось проверить'); }
  bindControls();updateClocks();setInterval(updateClocks,1000);
  try {
    const [config,status,events]=await Promise.all([api('/api/settings'),api('/api/status'),api('/api/events?limit=100')]);
    state.recent=events.items;setConfig(config,true);renderRecent();state.status=status;
    navigate();applyStatus(status);serverConnection(true);connectEvents();
  } catch(error) {
    serverConnection(false);placeholder('Не удалось загрузить панель',error.message);toast(error.message,true);
  }
}

initialize();
