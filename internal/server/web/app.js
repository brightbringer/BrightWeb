const state = {
  status: null,
  vault: null,
  files: [],
  viewMode: 'grid',
  selectedIds: new Set(),
  lastSelectedId: null,
  activeFile: null,
  filters: {
    query: '', folder_id: '', recursive: true, collection_id: '',
    tag: '', date_from: '', date_to: '', only_no_date: false,
    media_type: '', container_id: '', sort_by: 'date_desc'
  },
  uploadMode: 'native',
  pendingUploadFiles: [],
  pendingNativePaths: [],
  activePeekJobId: null,
  lastPeekSource: '',
  lastPeekManifest: null,
  peekSelectedRels: new Set(),
  lastSyncAnalysis: null,
  jobsTimer: null,
  heartbeatTimer: null,
  autoUnlockTried: false,
};

const $ = (id) => document.getElementById(id);

async function api(path, opts = {}) {
  const res = await fetch(path, {
    credentials: 'same-origin',
    ...opts,
    headers: {
      ...(opts.body && !(opts.body instanceof FormData) ? { 'Content-Type': 'application/json' } : {}),
      ...(opts.headers || {}),
    },
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `Erro HTTP ${res.status}`);
  return data;
}

function toast(msg, ms = 3400) {
  const el = $('toast');
  el.textContent = msg;
  el.classList.remove('hidden');
  clearTimeout(el._t);
  el._t = setTimeout(() => el.classList.add('hidden'), ms);
}

function fmtBytes(b) {
  if (!b) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, n = b;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i === 0 ? 0 : 1)} ${u[i]}`;
}

function esc(s) {
  return String(s || '').replace(/[&<>"']/g, m => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[m]));
}

function fmtETA(secs) {
  if (!isFinite(secs) || secs <= 0) return 'calculando...';
  secs = Math.ceil(secs);
  if (secs < 60) return `~${secs}s restantes`;
  const m = Math.floor(secs / 60), s = secs % 60;
  if (m < 60) return `~${m}m ${String(s).padStart(2, '0')}s restantes`;
  const h = Math.floor(m / 60);
  return `~${h}h ${String(m % 60).padStart(2, '0')}m restantes`;
}

// --- BOOTSTRAP ---
async function init() {
  bindEvents();
  startHeartbeat();
  await refreshStatus();
}

function startHeartbeat() {
  clearInterval(state.heartbeatTimer);
  let failCount = 0;
  const ping = async () => {
    try {
      const r = await fetch('/api/heartbeat', { method: 'POST', credentials: 'same-origin' });
      if (r.ok) {
        failCount = 0;
        const b = $('conn-lost-banner');
        if (b) b.remove();
        return;
      }
    } catch { }
    failCount++;
    if (failCount >= 3 && !$('conn-lost-banner')) {
      const div = document.createElement('div');
      div.id = 'conn-lost-banner';
      div.style.cssText = 'position:fixed;top:0;left:0;right:0;z-index:9999;background:#dc2626;color:#fff;padding:10px 16px;text-align:center;font-weight:600;font-size:13px;box-shadow:0 4px 16px rgba(0,0,0,.4)';
      div.textContent = '⚠️ Conexão perdida com o processo BrightVault.exe — feche esta janela e abra o BrightVault.exe novamente.';
      document.body.prepend(div);
    }
  };
  if (typeof EventSource !== 'undefined' && !state.sseConn) {
    state.sseConn = new EventSource('/api/events', { withCredentials: true });
    state.sseConn.onopen = () => {
      failCount = 0;
      const b = $('conn-lost-banner');
      if (b) b.remove();
    };
  }
  ping();
  document.addEventListener('visibilitychange', () => { if (!document.hidden) ping(); });
  state.heartbeatTimer = setInterval(ping, 12000);
}

function renderUpdatePill(u) {
  const btn = $('bv-update-btn');
  if (!btn) return;
  const cur = (u && u.current_version) || state.status?.version || '0.701';
  state.updateStatus = u || { current_version: cur };
  if (u && u.update_available && u.latest_version) {
    btn.classList.add('outdated');
    btn.textContent = `⬆️ Atualizar v${u.latest_version}`;
    btn.title = `Nova versão v${u.latest_version} disponível no GitHub (atual: v${cur}). Clique para atualizar automaticamente!`;
  } else {
    btn.classList.remove('outdated');
    btn.textContent = `v${cur}`;
    btn.title = u && u.last_checked
      ? `BrightVault v${cur} (atualizado · verificado em ${u.last_checked.slice(0, 16).replace('T', ' ')}). Clique para verificar atualizações no GitHub.`
      : `BrightVault v${cur} · Clique para verificar atualizações no GitHub`;
  }
}

async function pollUpdateStatusOnce() {
  try {
    const u = await api('/api/update/status');
    renderUpdatePill(u);
  } catch { }
}

async function refreshStatus() {
  try {
    state.status = await api('/api/status');
    const t = state.status.tools;
    $('auth-tools-status').textContent = `BrightVault v${state.status.version || '0.701'} · FFmpeg: ${t.has_ffmpeg ? 'OK' : 'Ausente'} · yt-dlp: ${t.has_yt_dlp ? 'OK' : 'Ausente'} · JS: ${t.has_js_runtime ? 'OK' : 'Ausente'}`;
    renderUpdatePill(state.status.update);
    if (!state.updatePollScheduled) {
      state.updatePollScheduled = true;
      setTimeout(pollUpdateStatusOnce, 4500);
    }

    // Auto-unlock smoothly if "Lembrar-se" was enabled and user didn't explicitly click Lock
    if (!state.status.vault_unlocked && state.status.vault_exists && state.status.has_remembered_password && !state.status.explicitly_locked && !state.autoUnlockTried) {
      state.autoUnlockTried = true;
      try {
        await api('/api/vault/unlock-remembered', { method: 'POST' });
        state.status = await api('/api/status');
      } catch { }
    }

    if (state.status.vault_unlocked) {
      $('auth-screen').classList.add('hidden');
      $('app-shell').classList.remove('hidden');
      populateStaticSelects();
      updateToolsBanner();
      await refreshVault();
      startJobsPolling();
    } else {
      $('app-shell').classList.add('hidden');
      $('auth-screen').classList.remove('hidden');
      $('remembered-box').classList.toggle('hidden', !state.status.has_remembered_password);
      const usesKF = !!state.status.vault_uses_keyfile;
      $('unlock-keyfile-wrap').classList.toggle('hidden', !usesKF);
      if (usesKF) {
        const hint = state.status.vault_keyfile_hint ? ` (esperado: ${state.status.vault_keyfile_hint})` : '';
        $('unlock-keyfile-label').textContent = `🔑 Arquivo-Chave Local (2º Fator obrigatório${hint})`;
      }
      if (!state.status.vault_exists) {
        switchAuthTab('create');
      } else {
        switchAuthTab('unlock');
      }
    }
  } catch (e) {
    showAuthError(e.message);
  }
}

function updateToolsBanner() {
  const t = state.status.tools;
  if (!t.has_ffmpeg || !t.has_yt_dlp) {
    $('tools-warning').classList.remove('hidden');
    const miss = [!t.has_ffmpeg && 'FFmpeg', !t.has_yt_dlp && 'yt-dlp'].filter(Boolean).join(' e ');
    $('tools-warning-text').textContent = `Faltando: ${miss} (necessário para 8K / YouTube)`;
  } else {
    $('tools-warning').classList.add('hidden');
  }
}

function populateStaticSelects() {
  const resOpts = state.status.resolutions.map(r => `<option value="${r.code}">${esc(r.label)}</option>`).join('');
  const allProfOpts = state.status.profiles.map(p => `<option value="${p.code}">${esc(p.name)}</option>`).join('');
  const dataProfs = state.status.profiles.filter(p => !p.art_mode);
  const dataProfOpts = dataProfs.map(p => `<option value="${p.code}">${esc(p.name)}</option>`).join('');
  $('up-resolution').innerHTML = resOpts;
  if ($('up-carrier-res')) $('up-carrier-res').innerHTML = resOpts;
  $('set-res').innerHTML = resOpts;
  $('sync-res').innerHTML = resOpts;
  $('up-profile').innerHTML = dataProfOpts;
  $('set-prof').innerHTML = allProfOpts;
  $('sync-prof').innerHTML = allProfOpts;
  if ($('up-art-theme') && state.status.art_themes) {
    $('up-art-theme').innerHTML = state.status.art_themes.map(t => `<option value="${t.code}">${esc(t.name)}</option>`).join('');
  }
}

async function refreshVault() {
  state.vault = await api('/api/vault/state');
  state.status = await api('/api/status');
  $('vault-title').textContent = state.vault.vault_name;
  const st = state.status.stats || { files: 0, total_bytes: 0 };
  $('stats-badge').textContent = `${st.files} arquivo(s) · ${fmtBytes(st.total_bytes)}`;

  if (state.vault.settings?.theme && window.BV_THEME) {
    window.BV_THEME.apply(state.vault.settings.theme);
  }
  if (state.vault.settings?.language && window.BV_I18N) {
    window.BV_I18N.apply(state.vault.settings.language);
  }

  renderSidebar();
  fillFolderSelects();
  if (window._hydratePerfPanel) window._hydratePerfPanel();
  await runSearch();
  if (state.activeFile) {
    const refreshed = state.files.find(x => x.id === state.activeFile.id);
    if (refreshed) openInspector(refreshed);
  }
}

function containerStatusLabel(status) {
  if (status === 'staged') return '⏳ Pendente (Local)';
  if (status === 'uploaded') return '☁️ No YouTube';
  if (status === 'verified') return '✅ Verificado';
  return status;
}

function renderProfileCard() {
  const v = state.vault;
  const card = $('profile-card');
  const title = $('profile-card-title');
  const pill = $('profile-card-pill');
  const msg = $('profile-card-msg');
  const btn = $('btn-open-catalog');
  if (!card || !v) return;

  card.classList.remove('profile-none', 'profile-outdated', 'profile-synced');
  const st = v.profile_status || 'none';
  if (st === 'synced') {
    card.classList.add('profile-synced');
    title.textContent = '✅ Backup do Perfil';
    pill.textContent = `Atualizado (v${v.catalog_seq})`;
    msg.textContent = 'Suas pastas, datas editadas e links estão 100% sincronizados no YouTube.';
    btn.textContent = '☁️ Gerenciar Perfil';
    btn.className = 'btn sm full';
  } else if (st === 'outdated') {
    card.classList.add('profile-outdated');
    title.textContent = '⚠️ Backup do Perfil';
    pill.textContent = 'Desatualizado';
    msg.textContent = 'Há novos vídeos, pastas ou metadados editados que ainda não foram salvos no seu perfil!';
    btn.textContent = '🔄 Atualizar Perfil Agora';
    btn.className = 'btn primary sm full';
  } else {
    card.classList.add('profile-none');
    title.textContent = '☁️ Backup do Perfil';
    pill.textContent = 'Não criado';
    msg.textContent = 'Você deve criar o backup do seu perfil assim que fizer seus backups para restaurar tudo em qualquer PC.';
    btn.textContent = '☁️ Criar Backup do Perfil';
    btn.className = 'btn sm full';
  }
}

function renderPendingDownloadsBanner() {
  const list = state.vault?.pending_downloads || [];
  const banner = $('pending-dl-banner');
  if (!banner) return;
  if (!list.length) {
    banner.classList.add('hidden');
    return;
  }
  const first = list[0];
  banner.classList.remove('hidden');
  banner.innerHTML = `
    <span>⏸️ <b>${list.length} download(s) interrompido(s)</b> (último progresso: <b>${first.last_progress || 0}%</b>) — o progresso parcial foi salvo e pode continuar de onde parou!</span>
    <div style="display:flex;gap:6px;flex-wrap:wrap">
      <button class="btn primary sm" data-dl-resume="${esc(first.id)}">▶️ Retomar Download</button>
      <button class="btn danger-ghost sm" data-dl-cancel="${esc(first.id)}">✕ Descartar</button>
    </div>`;
}

function resolveCodecTag(prof, vcodec) {
  const c = (vcodec || 'auto').toLowerCase();
  if (c === 'h264' || c === 'h264-mp4') return 'H.264 .mp4';
  if (c === 'av1' || c === 'av1-mp4') return 'AV1 .mp4';
  if (c === 'av1-mkv') return 'AV1 .mkv';
  if (c === 'vp9' || c === 'vp9-webm') return 'VP9 .webm';
  if (c === 'hevc' || c === 'hevc-mp4') return 'HEVC .mp4';
  if (prof === 'y8-gray' || prof === 'y6-gray' || prof === 'y4-gray' || prof === 'y2-safe' ||
      prof === 'art-chroma' || prof === 'art-crystal' || prof === 'art-fractal3d' || prof === 'art-angles') return 'AV1 .mp4';
  return 'H.264 .mp4';
}

function isAV1Effective(prof, vcodec) {
  return resolveCodecTag(prof, vcodec).startsWith('AV1');
}

function isStaticImagePath(p) {
  const clean = String(p || '').trim().replace(/^["']|["']$/g, '').split(/[?#]/)[0].toLowerCase();
  return /\.(jpe?g|png|webp|bmp|tiff?|avif)$/i.test(clean);
}

function computeComboMetrics(res, prof, fps, vcodec = 'auto', maskIntensity = 'balanced', hideHeader = false, crossFEC = 'auto', hostSource = '') {
  // Empirical 0..10 attribute scores calibrated from our 4K / 2K / 1080p / 720p + AV1/VP9/H.264 + Mask Intensity + Static Image benchmarks!
  // Base safety minimum without Cross-Frame RS = 6.0/10 (rec 7.0/10); with Cross-Frame RS active = 5.0/10 (rec 6.0/10).
  fps = Number(fps) || 10;
  let size = 7, up = 7, dl = 7, safe = 8, art = 1, estUp = 5.3, estDl = 6.5;
  const tag = resolveCodecTag(prof, vcodec);
  const useAV1 = tag.startsWith('AV1');
  const useVP9 = tag.startsWith('VP9');
  const effHost = String(hostSource || '').trim();
  const isStaticImg = isStaticImagePath(effHost);
  const clamp10 = (v) => Math.max(1, Math.min(10, Math.round(v * 10) / 10));
  const clampInt = (v) => Math.max(1, Math.min(10, Math.round(v)));
  const fpsScale = (baseFPS, factorPerDouble) => Math.pow(baseFPS / Math.max(1, fps), factorPerDouble);

  // Smooth continuous safety penalties from FPS and Resolution:
  // Higher FPS gives each frame fewer bits on YouTube; higher resolution at the same FPS increases pixel rate (MP/s).
  // E.g. 720p@20fps is clearly safer than 2K@20fps, and 10fps > 15fps > 24fps > 30fps > 60fps everywhere!
  const fpsSafePen = Math.max(0, Math.log2(fps / 8)) * 1.15;
  const resSafePen = res === '4k' ? 1.2 : res === '2k' ? 0.7 : res === '1080p' ? 0.35 : 0.0;
  const extremeLowResCapPen = (res === '720p' && fps >= 48) ? 1.8 : 0.0;

  // Seamless Hidden Header trade-off (applies to art-stego, video-fractal, video-chroma, geometric & stealth masks):
  // Removing the top B&W bar boosts visual Aesthetics (+1.5), slightly reduces header margin (-0.4 safety) and usable area (~3-5% in 720p/1080p).
  const geoArtProfs = new Set(['art-fullres', 'art-crystal', 'art-fractal3d', 'art-angles', 'art-fractal-angles', 'art-bauhaus', 'art-constel', 'art-truchet']);
  const stealthProfs = new Set(['stealth-vhs', 'stealth-rain', 'stealth-film', 'stealth-stars']);
  const canHideHdr = (prof === 'art-stego' || prof === 'video-fractal' || prof === 'video-chroma' || prof === 'video-bw' || geoArtProfs.has(prof) || stealthProfs.has(prof));
  const effHideHdr = canHideHdr && !!hideHeader;
  const hdrSafeDelta = canHideHdr ? (effHideHdr ? -0.4 : 0.3) : 0;
  const hdrArtDelta = canHideHdr ? (effHideHdr ? 0.4 : -1.6) : 0;
  const hdrAreaFactor = effHideHdr ? (res === '720p' ? 1.05 : res === '1080p' ? 1.03 : res === '2k' ? 1.015 : 1.0) : 1.0;

  if (prof === 'y8-gray') {
    art = 1;
    const baseUp = res === '4k' ? 5.3 : res === '2k' ? 5.2 : 5.4;
    const baseDl = res === '4k' ? 5.8 : res === '2k' ? 6.5 : 7.4;
    estUp = +(baseUp * fpsScale(10, 0.08)).toFixed(1);
    estDl = +(baseDl * fpsScale(10, 0.25)).toFixed(1);
    size = clampInt(7.5 + (fps >= 15 ? 1 : 0) + (res === '2k' ? 0.5 : 0));
    up = clampInt(7.5 + (fps >= 15 ? 1.5 : 0.5));
    dl = clampInt(7 + (res === '4k' ? 2 : res === '2k' ? 1 : 0) + (fps >= 15 ? 1 : 0));
    safe = clamp10(9.2 - fpsSafePen - resSafePen);
    if (useAV1) { size = clampInt(size + 1.5); up = 10; dl = 10; safe = clamp10(safe + 0.6); estUp = +(estUp * 0.67).toFixed(1); estDl = +(estDl * 0.75).toFixed(1); }
    else if (useVP9) { size = clampInt(size + 1); up = 8; dl = 9; safe = clamp10(safe + 0.4); estUp = +(estUp * 0.80).toFixed(1); estDl = +(estDl * 0.85).toFixed(1); }
  } else if (prof === 'y6-gray') {
    art = 1;
    const baseUp = res === '720p' ? 4.9 : res === '1080p' ? 5.0 : 5.2;
    const baseDl = res === '4k' ? 7.2 : res === '2k' ? 7.6 : 8.4;
    estUp = +(baseUp * fpsScale(10, 0.10)).toFixed(1);
    estDl = +(baseDl * fpsScale(10, 0.28)).toFixed(1);
    size = clampInt(7 + (fps >= 20 ? 2 : fps >= 15 ? 1 : 0));
    up = clampInt(7 + (fps >= 20 ? 2 : fps >= 15 ? 1.5 : 1));
    dl = clampInt(7 + (fps >= 20 ? 2.5 : fps >= 15 ? 1.5 : 0.5));
    safe = clamp10(9.8 - fpsSafePen - resSafePen);
    if (useAV1) { size = clampInt(size + 1.5); up = 10; dl = 10; safe = clamp10(safe + 0.5); estUp = +(estUp * 0.69).toFixed(1); estDl = +(estDl * 0.72).toFixed(1); }
    else if (useVP9) { size = clampInt(size + 1); up = 8; dl = 9; safe = clamp10(safe + 0.4); estUp = +(estUp * 0.84).toFixed(1); estDl = +(estDl * 0.78).toFixed(1); }
  } else if (prof === 'y4-gray') {
    art = 1;
    const baseUp = res === '720p' ? 4.8 : res === '1080p' ? 5.0 : 5.3;
    const baseDl = res === '720p' ? 8.8 : res === '1080p' ? 9.2 : 9.8;
    estUp = +(baseUp * fpsScale(15, 0.15)).toFixed(1);
    estDl = +(baseDl * fpsScale(15, 0.25)).toFixed(1);
    size = clampInt(6.5 + (fps >= 60 ? 3.5 : fps >= 30 ? 2.5 : fps >= 24 ? 1.5 : fps >= 15 ? 0.5 : 0));
    up = clampInt(7 + (fps >= 60 ? 3 : fps >= 30 ? 2 : fps >= 24 ? 1.5 : 0.5));
    dl = clampInt(6.5 + (fps >= 30 ? 2 : fps >= 24 ? 1.5 : fps >= 15 ? 0.5 : 0));
    safe = clamp10(10.0 - fpsSafePen - resSafePen - extremeLowResCapPen);
    if (useAV1) { size = clampInt(size + 1); up = 10; dl = 10; safe = clamp10(safe + 0.5); estUp = +(estUp * 0.76).toFixed(1); estDl = +(estDl * 0.62).toFixed(1); }
    else if (useVP9) { size = clampInt(size + 1); up = 8; dl = 10; safe = clamp10(safe + 0.7); estUp = +(estUp * 0.90).toFixed(1); estDl = +(estDl * 0.56).toFixed(1); }
  } else if (prof === 'y2-safe') {
    art = 1;
    estUp = +(8.6 * fpsScale(15, 0.08)).toFixed(1);
    estDl = +(12.5 * fpsScale(15, 0.30)).toFixed(1);
    size = clampInt(3 + (fps >= 60 ? 3 : fps >= 30 ? 2 : fps >= 24 ? 1 : 0));
    up = clampInt(4 + (fps >= 30 ? 2 : fps >= 15 ? 1 : 0));
    dl = clampInt(4 + (fps >= 60 ? 3 : fps >= 30 ? 2 : fps >= 24 ? 1 : 0));
    safe = clamp10(10.5 - fpsSafePen * 0.55 - resSafePen * 0.5);
    if (useAV1) { size = clampInt(size + 2); up = clampInt(up + 2); dl = clampInt(dl + 1); estUp = +(estUp * 0.72).toFixed(1); estDl = +(estDl * 0.75).toFixed(1); }
  } else if (prof === 'art-stego' || prof === 'art-chroma' || prof === 'video-fractal' || prof === 'video-chroma' || prof === 'video-bw' || geoArtProfs.has(prof) || stealthProfs.has(prof)) {
    // Mask Intensity adjustments (subtle=0.25x, balanced=1x, strong=2x, max=4x data/frame)
    const mi = maskIntensity || 'balanced';
    const miSizeBoost = mi === 'max' ? 3.0 : mi === 'strong' ? 2.0 : mi === 'subtle' ? -1.5 : 0;
    const miSafeDelta = mi === 'max' ? -3.4 : mi === 'strong' ? -1.6 : mi === 'subtle' ? 1.1 : 0;
    const miArtDelta  = mi === 'max' ? -2.8 : mi === 'strong' ? -1.2 : mi === 'subtle' ? 0.8 : 0;
    const miEstFactor = (mi === 'max' ? 0.36 : mi === 'strong' ? 0.58 : mi === 'subtle' ? 3.1 : 1.0) * hdrAreaFactor;

    if (stealthProfs.has(prof)) {
      // Stealth optical masks: prioritize 10/10 human invisibility (zero grid) with lower payload density
      const stSpec = {
        'stealth-vhs':   { upH: 75,  dl: 65,  safe: 9.8, art: 9.8 },
        'stealth-stars': { upH: 78,  dl: 64,  safe: 9.9, art: 10.0 },
        'stealth-rain':  { upH: 92,  dl: 78,  safe: 9.5, art: 10.0 },
        'stealth-film':  { upH: 85,  dl: 70,  safe: 7.4, art: 10.0 },
      }[prof];
      const stAmpSafe = mi === 'max' ? 0.8 : mi === 'strong' ? 0.4 : mi === 'subtle' ? -0.6 : 0;
      const stAmpArt  = mi === 'max' ? -1.2 : mi === 'strong' ? -0.5 : mi === 'subtle' ? 0.4 : 0;
      const resFactor = res === '4k' ? 0.85 : res === '2k' ? 0.92 : res === '1080p' ? 0.96 : 1.15;
      estUp = +(stSpec.upH * resFactor * fpsScale(15, 0.12)).toFixed(1);
      estDl = +(stSpec.dl * resFactor * fpsScale(15, 0.16)).toFixed(1);
      size = clampInt(2.0 + (res === '4k' ? 1 : 0) + (fps >= 30 ? 1 : 0));
      up = clampInt(2.5 + (fps >= 24 ? 1 : 0));
      dl = clampInt(3.0 + (fps >= 24 ? 1 : 0));
      safe = clamp10(stSpec.safe - fpsSafePen * 0.5 - resSafePen * 0.4 + stAmpSafe + (effHideHdr ? 0 : 0.2));
      art = clamp10(stSpec.art + stAmpArt + (effHideHdr ? 0 : -1.8));
    } else if (geoArtProfs.has(prof)) {
      const geoSpec = {
        'art-fullres':        { upH: 7.4,  upA: 9.6,  dl: 8.1,  safe: 9.2, art: 9.7, av1Good: false },
        'art-crystal':        { upH: 7.0,  upA: 6.9,  dl: 7.1,  safe: 9.2, art: 8.6, av1Good: true },
        'art-fractal3d':      { upH: 7.8,  upA: 7.6,  dl: 7.3,  safe: 9.4, art: 8.9, av1Good: true },
        'art-constel':        { upH: 9.2,  upA: 23.5, dl: 18.6, safe: 9.5, art: 8.8, av1Good: false },
        'art-bauhaus':        { upH: 10.1, upA: 34.8, dl: 17.4, safe: 9.1, art: 9.0, av1Good: false },
        'art-angles':         { upH: 11.4, upA: 11.1, dl: 9.4,  safe: 8.8, art: 9.1, av1Good: true },
        'art-fractal-angles': { upH: 13.0, upA: 29.1, dl: 12.6, safe: 8.7, art: 9.5, av1Good: false },
        'art-truchet':        { upH: 18.3, upA: 31.2, dl: 19.4, safe: 8.6, art: 9.6, av1Good: false },
      }[prof];
      const geoFactor = (mi === 'subtle' ? 2.4 : 1.0) * hdrAreaFactor;
      const baseUp = useAV1 ? geoSpec.upA : geoSpec.upH;
      estUp = +(baseUp * fpsScale(15, 0.14) * geoFactor).toFixed(1);
      estDl = +(geoSpec.dl * fpsScale(15, 0.20) * geoFactor).toFixed(1);
      size = clampInt(10.5 - Math.log2(Math.max(3, estUp)) * 1.9);
      up = clampInt(10.5 - Math.log2(Math.max(3, estUp)) * 1.7);
      dl = clampInt(10.5 - Math.log2(Math.max(3, estDl)) * 1.6);
      safe = clamp10(geoSpec.safe - fpsSafePen * 0.9 - resSafePen * 0.8 + (mi === 'subtle' ? 1.1 : 0) + hdrSafeDelta);
      art = clamp10(geoSpec.art + (mi === 'subtle' ? 0.4 : 0) + hdrArtDelta);
    } else if (prof === 'video-bw') {
      // Classic B&W / Grayscale optical noise mask (high capacity & high safety!)
      const hasHost = ($('up-host-video')?.value.trim() || '') !== '';
      const baseUp = hasHost ? 11.5 : (res === '720p' ? 4.6 : 5.2);
      const baseDl = hasHost ? 14.0 : (res === '720p' ? 7.4 : 7.8);
      const bwFactor = hasHost ? miEstFactor : (mi === 'subtle' ? 1.55 : mi === 'strong' ? 0.92 : mi === 'max' ? 0.86 : 1.0);
      estUp = +(baseUp * fpsScale(15, 0.14) * bwFactor).toFixed(1);
      estDl = +(baseDl * fpsScale(15, 0.22) * bwFactor).toFixed(1);
      size = clampInt((hasHost ? 5 : 8) + (fps >= 24 ? 1 : 0) + (hasHost ? miSizeBoost * 0.7 : (mi === 'subtle' ? -2 : 0)));
      up = clampInt((hasHost ? 5 : 8.5) + (fps >= 24 ? 1 : 0));
      dl = clampInt((hasHost ? 5 : 8) + (fps >= 24 ? 1 : 0));
      const bwSafeBase = hasHost ? 9.9 : (mi === 'subtle' ? 10.4 : mi === 'max' ? 9.1 : 9.7);
      safe = clamp10(bwSafeBase - fpsSafePen * 0.9 - resSafePen * 0.85 + (hasHost ? miSafeDelta * 0.85 : 0) + (hasHost ? hdrSafeDelta : 0));
      art = clamp10((hasHost ? 5.5 : 2.0) + (hasHost ? hdrArtDelta * 0.4 : 0));
      if (!hasHost && useAV1) { size = clampInt(size + 1.5); up = 10; dl = 10; estUp = +(estUp * 0.72).toFixed(1); estDl = +(estDl * 0.72).toFixed(1); }
    } else if (prof === 'art-stego') {
      const baseUp = res === '4k' ? 12.7 : res === '2k' ? 14.1 : res === '1080p' ? 13.8 : 16.2;
      const baseDl = res === '4k' ? 16.0 : res === '2k' ? 22.0 : res === '1080p' ? 18.0 : 17.5;
      estUp = +(baseUp * fpsScale(10, 0.16) * miEstFactor).toFixed(1);
      estDl = +(baseDl * fpsScale(10, 0.24) * miEstFactor).toFixed(1);
      size = clampInt((res === '4k' ? 3 : 2) + (fps >= 20 ? 2 : fps >= 15 ? 1 : fps <= 8 ? -0.5 : 0) + miSizeBoost);
      up = clampInt((res === '4k' ? 4 : 3) + (fps >= 20 ? 2 : fps >= 15 ? 1 : 0) + miSizeBoost * 0.8);
      dl = clampInt((res === '4k' ? 5 : 4) + (fps >= 20 ? 2 : fps >= 15 ? 1 : fps <= 8 ? -0.5 : 0) + miSizeBoost * 0.8);
      safe = clamp10(9.8 - fpsSafePen - resSafePen + miSafeDelta + hdrSafeDelta);
      art = clamp10(9.4 + miArtDelta + hdrArtDelta);
      if (useAV1) { size = clampInt(size - 1); estUp = +(estUp * 1.32).toFixed(1); }
    } else if (prof === 'art-chroma') {
      const baseUp = useAV1 ? 5.0 : (res === '720p' ? 7.5 : 6.1);
      const baseDl = useAV1 ? 6.3 : (res === '720p' ? 10.5 : 9.5);
      const acFactor = mi === 'max' ? 0.72 : mi === 'strong' ? 0.86 : mi === 'subtle' ? 3.0 : 1.0;
      estUp = +(baseUp * fpsScale(15, 0.14) * acFactor).toFixed(1);
      estDl = +(baseDl * fpsScale(15, 0.22) * acFactor).toFixed(1);
      size = clampInt((useAV1 ? 8 : 6) + (fps >= 20 ? 1 : 0) + miSizeBoost * 0.5);
      up = clampInt((useAV1 ? 8 : 6) + (fps >= 20 ? 1 : 0) + miSizeBoost * 0.4);
      dl = clampInt((useAV1 ? 8 : 6) + (fps >= 20 ? 1 : 0) + miSizeBoost * 0.4);
      safe = clamp10((useAV1 ? 9.6 : 9.3) - fpsSafePen - resSafePen + miSafeDelta * 0.9);
      art = clamp10(7.4 + miArtDelta * 0.8);
    } else if (prof === 'video-fractal') {
      const baseUp = res === '720p' ? 56.0 : res === '1080p' ? 44.0 : 40.0;
      const baseDl = res === '720p' ? 36.0 : res === '1080p' ? 29.0 : 26.0;
      estUp = +(baseUp * fpsScale(15, 0.22) * miEstFactor).toFixed(1);
      estDl = +(baseDl * fpsScale(15, 0.26) * miEstFactor).toFixed(1);
      size = clampInt((fps >= 30 ? 3.5 : fps >= 24 ? 2.5 : fps >= 15 ? 1.8 : 1) + miSizeBoost);
      up = clampInt((fps >= 30 ? 4 : fps >= 24 ? 3 : fps >= 15 ? 2 : 1) + miSizeBoost * 0.8);
      dl = clampInt((fps >= 30 ? 4.5 : fps >= 24 ? 3.5 : fps >= 15 ? 2.5 : 1.5) + miSizeBoost * 0.8);
      safe = clamp10(10.0 - fpsSafePen * 0.95 - resSafePen * 0.85 + miSafeDelta + hdrSafeDelta);
      art = clamp10(9.6 + miArtDelta + hdrArtDelta);
    } else {
      const baseUp = res === '720p' ? 53.0 : res === '1080p' ? 42.0 : 38.0;
      const baseDl = res === '720p' ? 34.0 : res === '1080p' ? 28.0 : 25.0;
      estUp = +(baseUp * fpsScale(15, 0.22) * miEstFactor).toFixed(1);
      estDl = +(baseDl * fpsScale(15, 0.26) * miEstFactor).toFixed(1);
      size = clampInt((fps >= 30 ? 4 : fps >= 24 ? 3 : fps >= 15 ? 2 : 1.2) + miSizeBoost);
      up = clampInt((fps >= 30 ? 4 : fps >= 24 ? 3 : fps >= 15 ? 2 : 1.2) + miSizeBoost * 0.8);
      dl = clampInt((fps >= 30 ? 4.5 : fps >= 24 ? 3.5 : fps >= 15 ? 2.5 : 1.5) + miSizeBoost * 0.8);
      safe = clamp10(10.0 - fpsSafePen * 0.9 - resSafePen * 0.8 + miSafeDelta * 0.9 + hdrSafeDelta);
      art = clamp10(8.0 + miArtDelta * 0.8 + hdrArtDelta);
    }
  } else if (prof === 'ycbcr-hex') {
    art = 3; size = 6; up = 7; dl = 6; safe = clamp10(7.2 - fpsSafePen - resSafePen); estUp = 6.7; estDl = 10.2;
  } else if (prof === 'ycbcr-dual') {
    art = 3; size = 4; up = 5; dl = 5; safe = clamp10(6.4 - fpsSafePen - resSafePen); estUp = 8.2; estDl = 11.5;
  } else if (prof === 'ycbcr-fast') {
    art = 3; size = 3; up = 5; dl = 4; safe = 2; estUp = 11.0; estDl = 14.0;
  } else {
    art = 1; size = 4; up = 4; dl = 4; safe = clamp10(8.0 - fpsSafePen - resSafePen); estUp = 9.0; estDl = 13.0;
  }
  // Static Image Background effect (real YouTube VP9/H.264 VBR behavior!):
  // Zero motion across the video lets YouTube drop bitrate aggressively (~1.4 Mbps in 1440p),
  // which compresses smaller (+size) but hurts fine high-frequency grain severely (especially stealth-film 16x16)!
  let staticImgWarn = '';
  if (isStaticImg) {
    estUp = +(estUp * 0.84).toFixed(1);
    estDl = +(estDl * 0.88).toFixed(1);
    size = clampInt(size + 0.6);
    if (prof === 'stealth-film') {
      safe = clamp10(safe - 2.4);
      staticImgWarn = '🖼️ <b>Imagem estática + Cinema 35mm:</b> o YouTube derruba o bitrate em fundos parados e apaga o grão fino 16×16! Use <b>Estrelas (stealth-stars)</b>, <b>VHS (stealth-vhs)</b> ou um <b>vídeo com movimento</b>.';
    } else if (prof === 'stealth-rain') {
      safe = clamp10(safe - 1.0);
    } else if (prof === 'stealth-stars' || prof === 'stealth-vhs') {
      safe = clamp10(safe - 0.3);
    } else if (prof === 'art-stego') {
      safe = clamp10(safe - 0.8);
    } else if (prof.startsWith('art-') || prof.startsWith('video-')) {
      safe = clamp10(safe - 0.5);
    }
  }

  // Parallel Decode 4x + Parallel yt-dlp (-N 8) & Partitioned Encode 4x (VP9/AV1) speed boosts
  dl = clampInt(dl + 1.2);
  if (useVP9 || useAV1) {
    up = clampInt(up + 0.8);
  }
  // Dynamic Cross-Frame RS Shield effect (does NOT reduce Art score — only adds parity frames & lowers min safe threshold!)
  const isArtOrStealth = prof.startsWith('stealth-') || prof.startsWith('art-') || prof.startsWith('video-');
  const cfMode = (crossFEC || 'auto').toLowerCase();
  let cfActive = false, cfOverhead = 1.0, cfTag = '';
  if (cfMode === 'max') {
    cfActive = true; cfOverhead = 1.33; cfTag = '🛡️ Blindagem Máxima (K=6,M=2 · 25% apagão)';
  } else if (cfMode === 'high') {
    cfActive = true; cfOverhead = 1.25; cfTag = '🛡️ Blindagem Alta (K=8,M=2 · 20% apagão)';
  } else if (cfMode === 'auto' && isArtOrStealth) {
    cfActive = true; cfOverhead = 1.33; cfTag = '🛡️ Blindagem Auto (K=6,M=2 · 25% apagão)';
  }
  const safeMin = cfActive ? 5.0 : 6.0;
  const safeRec = cfActive ? 6.0 : 7.0;
  if (cfActive) {
    estUp = +(estUp * cfOverhead).toFixed(1);
    estDl = +(estDl * cfOverhead).toFixed(1);
    size = clampInt(size - (cfOverhead > 1.3 ? 0.8 : 0.5));
  }
  return { size, up, dl, safe, art, estUp, estDl, codecLabel: tag, fps, safeMin, safeRec, canHideHdr, effHideHdr, cfActive, cfTag, isStaticImg, staticImgWarn };
}

function renderAttrBars(containerId, res, prof, fps, vcodec = 'auto', maskIntensity = 'balanced', hideHeader = false, crossFEC = 'auto', hostSource = '') {
  const el = $(containerId);
  if (!el) return;
  const m = computeComboMetrics(res, prof, fps || 10, vcodec, maskIntensity, hideHeader, crossFEC, hostSource);
  const barColor = (v, isSafe) => {
    if (isSafe) return v >= m.safeRec ? '#22c55e' : v >= m.safeMin ? '#f59e0b' : '#ef4444';
    return v >= 8 ? '#22c55e' : v >= 5 ? '#38bdf8' : v >= 3 ? '#f59e0b' : '#ef4444';
  };
  const fmtScore = (v) => Number.isInteger(v) ? String(v) : v.toFixed(1).replace('.', ',');
  const rows = [
    ['📦 Tamanho (Menor MP4)', m.size, false],
    ['⬆️ Velocidade Upload', m.up, false],
    ['⬇️ Velocidade Download', m.dl, false],
    ['🛡️ Segurança (Anti-Erro)', m.safe, true],
    ['🎨 Nível Artístico', m.art, false],
  ];
  const safeBadge = m.safe >= m.safeRec
    ? `<span style="color:#4ade80;font-weight:600">✅ Recomendado (≥${m.safeRec.toFixed(1).replace('.', ',')}/10${m.cfActive ? ' com Blindagem Cross-Frame' : ' — 0 perda mesmo sob compressão severa'})</span>`
    : m.safe >= m.safeMin
      ? `<span style="color:#fbbf24;font-weight:600">⚠️ Zona mínima (${m.safeMin.toFixed(1).replace('.', ',')}–${m.safeRec.toFixed(1).replace('.', ',')}/10) — passa em bitrate normal${m.cfActive ? ' (protegido contra apagões até 25%)' : ', pouca folga sob compressão severa'}</span>`
      : `<span style="color:#f87171;font-weight:700">❌ Abaixo do mínimo (${m.safeMin.toFixed(1).replace('.', ',')}/10) — risco real de perda de dados no YouTube!</span>`;
  const hdrTag = m.canHideHdr
    ? (m.effHideHdr ? ' · 🙈 <b>Sem barra P&amp;B</b> (tela cheia limpa)' : ' · ▬ <b>Com barra P&amp;B</b> (+segurança cabeçalho)')
    : '';
  const cfHtml = m.cfTag ? ` · <b>${m.cfTag}</b>` : '';
  const staticTag = m.isStaticImg ? ' · 🖼️ <b>Fundo Estático</b> (YouTube reduz bitrate)' : '';
  const staticWarnHtml = m.staticImgWarn ? `<div style="margin-top:4px;color:#fca5a5;font-weight:600">${m.staticImgWarn}</div>` : '';
  el.innerHTML = rows.map(([lbl, val, isSafe]) => `
    <div class="attr-row" style="${isSafe ? 'margin-top:8px' : ''}">
      <span class="attr-lbl">${lbl}</span>
      <div class="attr-track">
        <div class="attr-fill" style="width:${val * 10}%;background:${barColor(val, isSafe)}"></div>
        ${isSafe ? `<div class="attr-min-mark" style="left:${m.safeMin * 10}%" data-lbl="${m.safeMin} Mín" title="Nível mínimo (${m.safeMin}/10) — abaixo disso há risco real de perda"></div><div class="attr-rec-mark" style="left:${m.safeRec * 10}%" data-lbl="${m.safeRec} Rec" title="Nível recomendado (${m.safeRec}/10) — margem segura mesmo sob compressão severa"></div>` : ''}
      </div>
      <span class="attr-val" style="${isSafe ? (val < m.safeMin ? 'color:#f87171' : val < m.safeRec ? 'color:#fbbf24' : 'color:#4ade80') : ''}">${fmtScore(val)}/10</span>
    </div>`).join('') +
    `<div class="attr-summary">📊 <b>${m.codecLabel}</b> · <b>${res.toUpperCase()}</b> · <b>${m.fps} FPS</b>${hdrTag}${cfHtml}${staticTag} · p/ <b>1 GB</b>: <b>~${m.estUp.toFixed(1)} GB</b> up · <b>~${m.estDl.toFixed(1)} GB</b> down<br>${safeBadge}${staticWarnHtml}</div>`;
}

function updateProfileDesc(selectId, descId) {
  const sel = $(selectId);
  const el = $(descId);
  if (!sel || !el || !state.status?.profiles) return;
  const p = state.status.profiles.find(x => x.code === sel.value);
  el.textContent = p ? `ℹ️ ${p.description}` : '';
}

function renderSidebar() {
  const v = state.vault;
  renderProfileCard();
  renderPendingDownloadsBanner();
  // Folders: single Edit (✏️) button + drag-and-drop sibling reordering (strictly within same parent!)
  const fHtml = [`
    <div class="folder-row" data-folder-row="root" data-parent-id="">
      <button class="side-item ${!state.filters.folder_id && !state.filters.collection_id && !state.filters.container_id ? 'active' : ''}" data-folder=""><span>🏠 Todos os Arquivos</span></button>
      <div class="folder-acts">
        <button data-folder-props="root" title="Editar pasta raiz, ver chave e compartilhar">✏️</button>
      </div>
    </div>`];
  const byParent = {};
  for (const f of v.folders) {
    const p = f.parent_id || '';
    (byParent[p] = byParent[p] || []).push(f);
  }
  function walk(pid, depth) {
    for (const f of (byParent[pid] || [])) {
      if (f.id === 'root') { walk('root', 0); continue; }
      const pad = '&nbsp;'.repeat(depth * 3);
      const act = state.filters.folder_id === f.id ? 'active' : '';
      const normParent = f.parent_id || 'root';
      fHtml.push(`
        <div class="folder-row" draggable="true" data-folder-row="${f.id}" data-parent-id="${normParent}" title="Arraste para reordenar entre pastas irmãs">
          <button class="side-item ${act}" data-folder="${f.id}"><span>${pad}📁 ${esc(f.name)}</span></button>
          <div class="folder-acts">
            <button data-folder-props="${f.id}" title="Editar pasta (renomear, Pasta Mãe, chave, compartilhar e excluir)">✏️</button>
          </div>
        </div>`);
      walk(f.id, depth + 1);
    }
  }
  walk('', 0);
  $('folder-tree').innerHTML = fHtml.join('');

  // Collections
  $('collection-list').innerHTML = v.collections.length
    ? v.collections.map(c => `<button class="side-item ${state.filters.collection_id === c.id ? 'active' : ''}" data-col="${c.id}"><span><span style="color:${esc(c.color)}">●</span> ${esc(c.name)}</span></button>`).join('')
    : '<div class="muted sm-txt" style="padding:4px 8px">Nenhuma coleção</div>';

  // Years
  $('year-list').innerHTML = v.years.map(y => `<button class="pill ${state.filters.date_from === y && state.filters.date_to === y ? 'active' : ''}" data-year="${y}">${y}</button>`).join('');

  // Tags
  $('tag-list').innerHTML = v.tags.length
    ? v.tags.map(t => `<button class="pill ${state.filters.tag === t ? 'active' : ''}" data-tag="${esc(t)}">#${esc(t)}</button>`).join('')
    : '<span class="muted sm-txt">Sem tags</span>';

  // Pending-YouTube Banner (for staged local videos not yet sent to YouTube!)
  const stagedList = v.containers.filter(c => c.status === 'staged' && !c.local_deleted && c.local_video_path);
  const banner = $('pending-yt-banner');
  if (stagedList.length > 0) {
    banner.classList.remove('hidden');
    const first = stagedList[0];
    banner.innerHTML = `
      <span>⏳ <b>${stagedList.length} vídeo(s) pronto(s) localmente</b> aguardando envio para o YouTube (ex.: <code>${esc(first.short_code)}</code> · ${fmtBytes(first.video_size_bytes)}).</span>
      <div style="display:flex;gap:6px;flex-wrap:wrap">
        <button class="btn primary sm" data-cont-upload="${first.id}">🚀 Enviar p/ YouTube (API)</button>
        <button class="btn sm" data-cont-open="${first.id}">📂 Abrir .mp4 (Envio Manual)</button>
        <button class="btn sm" data-cont-link="${first.id}">🔗 Colar Link do YT</button>
      </div>`;
  } else {
    banner.classList.add('hidden');
  }

  // Videos Summary Card (sidebar) + live refresh of Meus Vídeos modal if open
  const pendingConts = v.containers.filter(c => c.status === 'staged');
  const uploadableConts = v.containers.filter(c => c.status === 'staged' && !c.local_deleted && c.local_video_path);
  const sentConts = v.containers.filter(c => c.status === 'uploaded' || c.status === 'verified');
  if ($('vsum-pending-count')) $('vsum-pending-count').textContent = String(pendingConts.length);
  if ($('vsum-sent-count')) $('vsum-sent-count').textContent = String(sentConts.length);
  const upAllBtn = $('btn-upload-all-videos');
  if (upAllBtn) {
    upAllBtn.disabled = uploadableConts.length === 0;
    upAllBtn.textContent = uploadableConts.length > 0 ? `🚀 Enviar Todos (${uploadableConts.length})` : '🚀 Enviar Todos';
  }
  if ($('modal-my-videos') && !$('modal-my-videos').classList.contains('hidden')) {
    renderMyVideosModal();
  }
}

function privacyBadgeHTML(priv) {
  if (priv === 'public') return '<span class="badge-priv-public">🌍 Público</span>';
  if (priv === 'private') return '<span class="badge-priv-private">🔒 Privado</span>';
  return '<span class="badge-priv-unlisted">🔗 Não Listado</span>';
}

function renderMyVideoCard(c, isDelta = false) {
  const priv = c.privacy || 'unlisted';
  const hasLocal = !c.local_deleted && !!c.local_video_path;
  const syncBadge = c.sync_id
    ? (c.delta_index > 0
      ? `<span class="badge-delta">🔄 Atualização #${c.delta_index}</span>`
      : `<span class="badge-base">📦 Backup Base</span>`)
    : '';
  return `
    <div class="mv-card ${isDelta ? 'is-delta' : ''}" data-mv-id="${c.id}">
      <div class="mv-head">
        <div class="mv-title-grp">
          <strong>${esc(c.short_code)}</strong>
          ${syncBadge}
          <span class="status-dot status-${c.status}">${containerStatusLabel(c.status)}</span>
          ${privacyBadgeHTML(priv)}
        </div>
        <div class="muted" style="font-size:11px">${c.created_at ? esc(c.created_at.slice(0, 16).replace('T', ' ')) : ''}</div>
      </div>
      <div class="mv-meta">
        <span>🎞️ ${esc((c.resolution || '').toUpperCase())} · ${esc(c.profile_code || '')}</span>
        <span>📄 ${c.file_count} arquivo(s)</span>
        <span>💾 Vídeo: ${fmtBytes(c.video_size_bytes)}</span>
        <span>📦 Dados: ${fmtBytes(c.data_size_bytes)}</span>
        ${c.youtube_url ? `<span>▶️ <a href="${esc(c.youtube_url)}" target="_blank" style="color:var(--primary)">${esc(c.youtube_video_id || 'YouTube')}</a></span>` : '<span class="muted">⏳ Sem link do YouTube</span>'}
      </div>
      <div class="mv-btns">
        <button class="btn sm" data-cont-filter="${c.id}" title="Filtrar os arquivos deste vídeo na grade">👁️ Ver Arquivos</button>
        ${c.status === 'staged' && hasLocal ? `<button class="btn sm primary" data-cont-upload="${c.id}" title="Enviar este vídeo pronto para o YouTube via API">🚀 Enviar YT</button>` : ''}
        ${hasLocal ? `<button class="btn sm" data-cont-open="${c.id}" title="Abrir arquivo .mp4 no Windows Explorer">📂 .mp4</button>` : ''}
        <button class="btn sm" data-cont-rename="${c.id}" title="Alterar o nome / título deste vídeo (também atualiza no YouTube se já enviado)">✏️ Nome</button>
        <button class="btn sm" data-cont-link="${c.id}">${c.youtube_video_id ? '🔗 Editar Link YT' : '🔗 Colar Link'}</button>
        ${c.youtube_video_id ? `
          <select class="mv-priv-sel" data-cont-privacy="${c.id}" title="Alterar privacidade no YouTube via API">
            <option value="public" ${priv === 'public' ? 'selected' : ''}>🌍 Público</option>
            <option value="unlisted" ${priv === 'unlisted' ? 'selected' : ''}>🔗 Não Listado</option>
            <option value="private" ${priv === 'private' ? 'selected' : ''}>🔒 Privado</option>
          </select>` : ''}
        ${c.status === 'uploaded' ? `<button class="btn sm" data-cont-verify="${c.id}" title="Verificar se o YouTube concluiu processamento">✅ Verificar</button>` : ''}
        <button class="btn sm" data-cont-extract="${c.id}" title="Baixar e extrair todos os arquivos deste vídeo">⬇️ Extrair</button>
        ${hasLocal ? `<button class="btn sm" data-cont-del="${c.id}" title="Apagar apenas o arquivo .mp4 temporário local do HD">🧹 Limpar .mp4</button>` : ''}
        <button class="btn sm" data-cont-disassoc="${c.id}" title="Desvincular arquivos deste vídeo sem excluí-los do catálogo">🔌 Desassociar Arquivos</button>
        <button class="btn sm danger-ghost" data-cont-remove="${c.id}" title="Deletar este vídeo do catálogo (e opcionalmente do HD/YouTube)">🗑️ Deletar Vídeo</button>
      </div>
    </div>`;
}

function renderMyVideosModal() {
  const listEl = $('my-videos-list');
  if (!listEl || !state.vault) return;
  const all = state.vault.containers || [];
  const pending = all.filter(c => c.status === 'staged');
  const sent = all.filter(c => c.status === 'uploaded' || c.status === 'verified');
  const uploadable = all.filter(c => c.status === 'staged' && !c.local_deleted && c.local_video_path);

  if ($('mv-cnt-all')) $('mv-cnt-all').textContent = String(all.length);
  if ($('mv-cnt-pending')) $('mv-cnt-pending').textContent = String(pending.length);
  if ($('mv-cnt-sent')) $('mv-cnt-sent').textContent = String(sent.length);
  if ($('btn-mv-upload-all')) {
    $('btn-mv-upload-all').disabled = uploadable.length === 0;
    $('btn-mv-upload-all').textContent = uploadable.length > 0 ? `🚀 Enviar Todos Pendentes (${uploadable.length})` : '🚀 Nenhum Pendente';
  }

  const filter = state.myVideosFilter || 'all';
  const matchesFilter = (c) => {
    if (filter === 'pending') return c.status === 'staged';
    if (filter === 'sent') return c.status === 'uploaded' || c.status === 'verified';
    return true;
  };

  // Group SyncGroup chains (Base + Update Delta videos) together alongside standalone containers!
  const sgMap = new Map((state.vault.sync_groups || []).map(g => [g.id, g]));
  const chainMap = new Map();
  const standalones = [];

  for (const c of all) {
    if (c.sync_id) {
      if (!chainMap.has(c.sync_id)) chainMap.set(c.sync_id, []);
      chainMap.get(c.sync_id).push(c);
    } else {
      standalones.push(c);
    }
  }

  const entries = [];
  for (const [syncId, members] of chainMap.entries()) {
    const filtered = members.filter(matchesFilter);
    if (!filtered.length) continue;
    filtered.sort((a, b) => (a.delta_index || 0) - (b.delta_index || 0) || (a.created_at || '').localeCompare(b.created_at || ''));
    const newest = members.reduce((acc, x) => (x.created_at > acc ? x.created_at : acc), '');
    entries.push({ kind: 'chain', syncId, sg: sgMap.get(syncId), items: filtered, totalMembers: members.length, sortAt: newest });
  }
  for (const c of standalones) {
    if (!matchesFilter(c)) continue;
    entries.push({ kind: 'single', item: c, sortAt: c.created_at || '' });
  }
  entries.sort((a, b) => (b.sortAt || '').localeCompare(a.sortAt || ''));

  if (!entries.length) {
    listEl.innerHTML = `<div class="inspector-empty" style="padding:36px 12px"><div class="empty-ico">🎬</div><h3>Nenhum vídeo nesta categoria</h3><p class="muted">Envie arquivos ou pastas em <b>+ Enviar</b> para gerar seus vídeos ópticos criptografados.</p></div>`;
    return;
  }

  listEl.innerHTML = entries.map(ent => {
    if (ent.kind === 'single') return renderMyVideoCard(ent.item, false);
    const base = ent.items[0];
    const deltas = ent.items.slice(1);
    const sgName = ent.sg?.name || ent.syncId;
    const maxD = ent.sg?.max_deltas ?? 10;
    const updCount = Math.max(0, ent.totalMembers - 1);
    return `
      <div class="vid-chain-box">
        <div class="vid-chain-head">
          <span>🔗 Cadeia de Sincronização: <b>${esc(sgName)}</b> <code style="font-weight:400">${esc(ent.syncId)}</code></span>
          <span class="muted" style="font-weight:500">${ent.totalMembers} vídeo(s) · ${updCount}/${maxD} atualizações</span>
        </div>
        ${renderMyVideoCard(base, (base.delta_index || 0) > 0)}
        ${deltas.length ? `<div class="vid-chain-deltas">${deltas.map(d => renderMyVideoCard(d, true)).join('')}</div>` : ''}
      </div>`;
  }).join('');
}

function fillFolderSelects() {
  const opts = state.vault.folders.map(f => `<option value="${f.id}">${f.id === 'root' ? '📁 Raiz (Meu BrightVault)' : '📁 ' + esc(f.name)}</option>`).join('');
  if ($('up-dest-folder')) $('up-dest-folder').innerHTML = opts;
  if ($('sync-dest-folder')) $('sync-dest-folder').innerHTML = opts;
  if ($('batch-folder')) $('batch-folder').innerHTML = `<option value="">(Manter pasta atual)</option>` + opts;
  if ($('batch-collection')) $('batch-collection').innerHTML = `<option value="">(Nenhuma)</option>` +
    state.vault.collections.map(c => `<option value="${c.id}">${esc(c.name)}</option>`).join('');
}

async function runSearch() {
  const res = await api('/api/files/search', { method: 'POST', body: JSON.stringify(state.filters) });
  state.files = res.files || [];
  updateBreadcrumb();
  renderFiles();
}

function updateBreadcrumb() {
  const parts = [];
  if (state.filters.folder_id) {
    const f = state.vault.folders.find(x => x.id === state.filters.folder_id);
    if (f) parts.push('📁 ' + f.name);
  }
  if (state.filters.collection_id) {
    const c = state.vault.collections.find(x => x.id === state.filters.collection_id);
    if (c) parts.push('⭐ ' + c.name);
  }
  if (state.filters.container_id) {
    const c = state.vault.containers.find(x => x.id === state.filters.container_id);
    if (c) parts.push('🎬 ' + c.short_code);
  }
  if (state.filters.tag) parts.push('#' + state.filters.tag);
  if (state.filters.only_no_date) parts.push('❓ Sem data');
  $('breadcrumb').textContent = (parts.length ? parts.join(' / ') : 'Todos os Arquivos') + ` (${state.files.length})`;
  if ($('btn-delete-current-folder')) {
    $('btn-delete-current-folder').classList.toggle('hidden', !state.filters.folder_id || state.filters.folder_id === 'root');
  }
}

function fileIcon(f) {
  if (f.preview_kind === 'image') return '🖼️';
  if (f.preview_kind === 'video') return '🎞️';
  if (f.preview_kind === 'text') return '📄';
  if (f.mime_type?.includes('pdf')) return '📕';
  if (f.mime_type?.startsWith('audio/')) return '🎵';
  return '📦';
}

function getFileSyncBadge(f) {
  if (!f.container_id) return { cls: 'staged', txt: '🔌 Sem vídeo' };
  const c = state.vault?.containers?.find(x => x.id === f.container_id);
  if (!c) return { cls: 'staged', txt: '⏳ Local' };
  if (c.status === 'verified') return { cls: 'verified', txt: '✅ YT' };
  if (c.status === 'uploaded') return { cls: 'uploaded', txt: '☁️ YT' };
  return { cls: 'staged', txt: '⏳ Só Local' };
}

function renderCard(f) {
  const sel = state.selectedIds.has(f.id) || state.activeFile?.id === f.id;
  const thumb = (f.preview_kind === 'image' || f.preview_kind === 'video') && f.preview_base64
    ? `<img src="data:image/jpeg;base64,${f.preview_base64}" loading="lazy">`
    : `<span>${fileIcon(f)}</span>`;
  const dateLbl = f.effective_date ? f.effective_date.slice(0, 10) : 'Sem data';
  const badge = getFileSyncBadge(f);
  return `
    <div class="file-card ${sel ? 'selected' : ''}" data-file-id="${f.id}">
      <input type="checkbox" class="card-chk" data-chk-id="${f.id}" ${state.selectedIds.has(f.id) ? 'checked' : ''}>
      <span class="card-sync ${badge.cls}" title="${badge.cls === 'staged' ? 'Vídeo gerado localmente — ainda NÃO enviado para o YouTube!' : 'Salvo no YouTube'}">${badge.txt}</span>
      <div class="card-thumb">${thumb}</div>
      <div class="card-body">
        <div class="card-name" title="${esc(f.display_name)}">${esc(f.display_name)}</div>
        <div class="card-meta"><span>${esc(dateLbl)}</span><span>${fmtBytes(f.size_bytes)}</span></div>
      </div>
    </div>`;
}

function renderFiles() {
  const vp = $('files-viewport');
  if (!state.files.length) {
    vp.innerHTML = `<div class="inspector-empty"><div class="empty-ico">☁️</div><h3>Nenhum arquivo encontrado</h3><p class="muted">Arraste arquivos ou pastas para cá, ou clique em <b>+ Enviar</b> para gerar seu primeiro vídeo criptografado em 8K.</p></div>`;
    return;
  }

  if (state.viewMode === 'grid') {
    vp.innerHTML = `<div class="file-grid">${state.files.map(renderCard).join('')}</div>`;
  } else if (state.viewMode === 'list') {
    vp.innerHTML = `
      <table class="file-table">
        <thead><tr><th style="width:28px"></th><th>Nome</th><th>Data Efetiva</th><th>Local</th><th>Tamanho</th><th>Tags</th></tr></thead>
        <tbody>
          ${state.files.map(f => `
            <tr class="${state.selectedIds.has(f.id) || state.activeFile?.id === f.id ? 'selected' : ''}" data-file-id="${f.id}">
              <td><input type="checkbox" data-chk-id="${f.id}" ${state.selectedIds.has(f.id) ? 'checked' : ''}></td>
              <td>${fileIcon(f)} ${esc(f.display_name)}</td>
              <td>${esc(f.effective_date || '—')} <span class="muted">(${esc(f.date_precision)})</span></td>
              <td>${esc(f.location_label || '—')}</td>
              <td>${fmtBytes(f.size_bytes)}</td>
              <td>${(f.tags || []).map(t => '#' + esc(t)).join(' ')}</td>
            </tr>`).join('')}
        </tbody>
      </table>`;
  } else {
    // Timeline grouped by Year-Month (or Year / Unknown)
    const groups = new Map();
    for (const f of state.files) {
      let key = 'Sem Data Definida';
      if (f.effective_date) {
        key = f.effective_date.length >= 7 ? f.effective_date.slice(0, 7) : f.effective_date.slice(0, 4);
      }
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(f);
    }
    let html = '';
    for (const [grp, items] of groups.entries()) {
      html += `<div class="tl-group"><div class="tl-header">📅 ${esc(grp)} (${items.length})</div><div class="file-grid">${items.map(renderCard).join('')}</div></div>`;
    }
    vp.innerHTML = html;
  }
  updateBatchBar();
}

function getTargetFileIds() {
  if (state.selectedIds.size > 0) return [...state.selectedIds];
  if (state.activeFile?.id) return [state.activeFile.id];
  return [];
}

function updateBatchBar() {
  const n = state.selectedIds.size;
  if (n > 0) {
    $('batch-bar').classList.remove('hidden');
    $('batch-count').textContent = `${n} selecionado(s)`;
  } else if (state.activeFile) {
    $('batch-bar').classList.remove('hidden');
    const nm = state.activeFile.display_name || '1 arquivo';
    $('batch-count').textContent = nm.length > 26 ? nm.slice(0, 25) + '…' : nm;
  } else {
    $('batch-bar').classList.add('hidden');
  }
}

function closeInspector() {
  state.activeFile = null;
  $('inspector-content').classList.add('hidden');
  $('inspector-empty').classList.remove('hidden');
  renderFiles();
}

function openInspector(file) {
  state.activeFile = file;
  $('inspector-empty').classList.add('hidden');
  $('inspector-content').classList.remove('hidden');
  if ($('insp-header-name')) {
    $('insp-header-name').textContent = file.display_name || file.original_name || 'Arquivo';
    $('insp-header-name').title = file.display_name || file.original_name || '';
  }

  const pb = $('preview-box');
  if ((file.preview_kind === 'image' || file.preview_kind === 'video') && file.preview_base64) {
    pb.innerHTML = `<img src="data:image/jpeg;base64,${file.preview_base64}" id="insp-preview-img" title="Clique para ampliar">`;
    $('insp-preview-img').onclick = () => {
      $('lightbox-img').src = `data:image/jpeg;base64,${file.preview_base64}`;
      $('lightbox').classList.remove('hidden');
    };
  } else if (file.preview_kind === 'text' && file.preview_base64) {
    let txt = '';
    try { txt = decodeURIComponent(escape(atob(file.preview_base64))); } catch { txt = atob(file.preview_base64); }
    pb.innerHTML = `<pre class="preview-text">${esc(txt)}</pre>`;
  } else {
    pb.innerHTML = `<div style="text-align:center;padding:20px"><div style="font-size:44px">${fileIcon(file)}</div><div class="muted">${esc(file.mime_type)}</div></div>`;
  }

  // Toggle post-download / local folder & container quick buttons
  const hasLocalCopy = !!(file.last_extracted_path || (file.local_source_path && !file.local_freed));
  if ($('btn-reveal-local')) $('btn-reveal-local').classList.toggle('hidden', !hasLocalCopy);
  if ($('btn-reveal-origin')) $('btn-reveal-origin').classList.toggle('hidden', !file.local_source_path);
  if ($('btn-open-container')) $('btn-open-container').classList.toggle('hidden', !file.container_id);

  const folderObj = state.vault.folders.find(x => x.id === (file.folder_id || 'root'));
  const colNames = (file.collection_ids || [])
    .map(cid => state.vault.collections.find(c => c.id === cid)?.name)
    .filter(Boolean);
  const precMap = { exact: 'Exata', month: 'Mês', year: 'Ano', decade: 'Década', unknown: 'Desconhecida' };
  if ($('inspector-summary')) {
    $('inspector-summary').innerHTML = `
      <div class="insp-title">${esc(file.display_name || file.original_name)}</div>
      <div class="insp-kv"><span>📅 Data</span><span>${esc(file.effective_date || 'Sem data')} ${file.effective_date ? `<span class="muted">(${precMap[file.date_precision] || file.date_precision || 'Exata'})</span>` : ''}</span></div>
      <div class="insp-kv"><span>📁 Pasta</span><span>${esc(folderObj ? folderObj.name : 'Raiz')}</span></div>
      ${file.location_label ? `<div class="insp-kv"><span>📍 Local</span><span>${esc(file.location_label)}</span></div>` : ''}
      ${(file.tags && file.tags.length) ? `<div class="insp-kv"><span>🏷️ Tags</span><span>${file.tags.map(t => '#' + esc(t)).join(' ')}</span></div>` : ''}
      ${colNames.length ? `<div class="insp-kv"><span>⭐ Coleções</span><span>${colNames.map(esc).join(', ')}</span></div>` : ''}
      ${file.notes ? `<div class="muted" style="font-size:11.5px;border-top:1px solid var(--border);padding-top:6px;white-space:pre-wrap">${esc(file.notes)}</div>` : ''}
    `;
  }

  const cont = state.vault.containers.find(c => c.id === file.container_id);
  $('meta-tech').innerHTML = `
    <div><b>Original:</b> ${esc(file.original_name)} (${fmtBytes(file.size_bytes)})</div>
    <div><b>SHA-256:</b> <code>${esc(file.sha256?.slice(0, 24))}…</code></div>
    <div><b>Contêiner:</b> ${cont ? `${esc(cont.short_code)} (${cont.resolution.toUpperCase()} ${cont.profile_code})` : (file.container_id ? '—' : '🔌 Desassociado')}</div>
    ${cont?.youtube_url ? `<div><b>YouTube:</b> <a href="${esc(cont.youtube_url)}" target="_blank" style="color:var(--primary)">${esc(cont.youtube_video_id)}</a> ${privacyBadgeHTML(cont.privacy)}</div>` : '<div class="muted">Aguardando vínculo com URL do YouTube</div>'}
    ${file.last_extracted_path ? `<div><b>Baixado em:</b> ${esc(file.last_extracted_path)}</div>` : ''}
    ${file.local_source_path ? `<div><b>Original local:</b> ${file.local_freed ? '✅ Espaço liberado' : esc(file.local_source_path)}</div>` : ''}
  `;
  updateBatchBar();
}

// --- JOBS POLLING ---
function startJobsPolling() {
  clearInterval(state.jobsTimer);
  state.jobsTimer = setInterval(pollJobs, 1500);
  pollJobs();
}

let lastCompletedJobIds = new Set();
async function pollJobs() {
  if (!state.status?.vault_unlocked) return;
  try {
    const { jobs } = await api('/api/jobs');
    if (!jobs || !jobs.length) return;
    const active = jobs.some(j => j.status === 'running');
    if (active) $('jobs-drawer').classList.remove('hidden');

    let needRefresh = false;
    for (const j of jobs) {
      if (state.activePeekJobId && j.id === state.activePeekJobId) {
        updatePeekProgressUI(j);
      }
      if ((j.status === 'done' || j.status === 'error') && !lastCompletedJobIds.has(j.id)) {
        lastCompletedJobIds.add(j.id);
        if (j.status === 'done') {
          needRefresh = true;
          if (j.kind === 'calibrate' && Array.isArray(j.result)) renderCalibrationResults(j.result);
          if (j.kind === 'peek' && j.result?.manifest) renderPeekResult(j.result);
          toast(`✅ ${j.title} concluído!`);
        } else {
          if (j.kind === 'peek') {
            $('peek-progress-box').classList.add('hidden');
            $('peek-result').innerHTML = `<div class="alert error">${esc(j.error)}</div>`;
            state.activePeekJobId = null;
          }
          toast(`❌ ${j.title}: ${j.error}`, 5500);
        }
      }
    }
    renderJobs(jobs);
    if (needRefresh) {
      await refreshVault();
      updateToolsBanner();
    }
  } catch { }
}

function renderJobs(jobs) {
  $('jobs-list').innerHTML = jobs.slice(0, 8).map(j => {
    const metaRight = [j.speed_text, j.eta_text].filter(Boolean).join(' · ');
    let postBtns = '';
    if (j.status === 'done' && (j.kind === 'decode' || j.kind === 'open_file') && j.result) {
      const fid = j.result.file_id || (Array.isArray(j.result.file_ids) && j.result.file_ids.length > 0 ? j.result.file_ids[0] : '');
      const cid = Array.isArray(j.result.container_ids) && j.result.container_ids.length > 0 ? j.result.container_ids[0] : '';
      if (fid) {
        postBtns = `
          <div class="job-post-actions">
            <button type="button" class="btn sm primary" data-job-file="${esc(fid)}" data-job-mode="open">📂 Abrir</button>
            <button type="button" class="btn sm" data-job-file="${esc(fid)}" data-job-mode="open_with">🔧 Abrir com...</button>
            <button type="button" class="btn sm" data-job-file="${esc(fid)}" data-job-mode="reveal_local">📁 Pasta Local</button>
            <button type="button" class="btn sm" data-job-file="${esc(fid)}" data-job-mode="reveal_origin">🗂️ Pasta de Origem</button>
            <button type="button" class="btn sm" data-job-file="${esc(fid)}" data-job-mode="open_container">🎬 Abrir Contêiner</button>
          </div>`;
      } else if (cid) {
        postBtns = `
          <div class="job-post-actions">
            <button type="button" class="btn sm" data-cont-open="${esc(cid)}">🎬 Abrir Contêiner</button>
          </div>`;
      }
    }
    return `
    <div class="job-row">
      <div style="display:flex;justify-content:space-between;gap:8px;align-items:center">
        <strong>${esc(j.title)}</strong>
        <span class="status-dot status-${j.status === 'done' ? 'verified' : j.status === 'error' ? 'staged' : 'uploaded'}">${j.status === 'running' ? j.progress + '%' : j.status}</span>
      </div>
      <div class="job-bar"><div class="job-bar-fill" style="width:${j.progress}%"></div></div>
      <div style="display:flex;justify-content:space-between;gap:8px" class="muted">
        <span>${esc(j.error || j.detail || (j.result?.dest_dir ? 'Salvo em: ' + j.result.dest_dir : ''))}</span>
        ${metaRight ? `<span style="color:var(--primary);white-space:nowrap">⏱ ${esc(metaRight)}</span>` : ''}
      </div>
      ${postBtns}
    </div>`;
  }).join('');
}

function updatePeekProgressUI(j) {
  const box = $('peek-progress-box');
  if (j.status === 'running') {
    box.classList.remove('hidden');
    $('peek-prog-detail').textContent = j.detail || 'Descompilando manifesto...';
    $('peek-prog-pct').textContent = `${j.progress}%`;
    $('peek-prog-bar').style.width = `${j.progress}%`;
    const meta = [j.speed_text, j.eta_text].filter(Boolean).join(' · ');
    $('peek-prog-eta').textContent = meta ? `⏱ ${meta}` : 'Processando em segundo plano...';
  } else {
    box.classList.add('hidden');
  }
}

function renderPeekResult(res) {
  state.activePeekJobId = null;
  state.lastPeekManifest = res.manifest;
  state.peekSelectedRels.clear();
  $('peek-progress-box').classList.add('hidden');
  const m = res.manifest;
  const syncMeta = m.sync_id ? ` · <code>${esc(m.sync_id)}</code> (${m.delta_index > 0 ? `Atualização ${m.delta_index}/${m.max_deltas || 10}` : 'Backup Base'})` : '';
  $('peek-result').innerHTML = `
    <div class="hint-box"><b>Contêiner:</b> ${esc(m.container_id)} · ${esc(res.resolution)} (${esc(res.profile_code)}) · ${m.files.length} arquivo(s) · ${fmtBytes(m.total_size)}${syncMeta}</div>
    <div class="file-grid">
      ${m.files.map(f => `
        <div class="file-card" data-peek-card="${esc(f.rel_path)}">
          <input type="checkbox" class="card-chk" data-peek-chk="${esc(f.rel_path)}">
          <div class="card-thumb">${f.preview_kind === 'image' && f.preview_base64 ? `<img src="data:image/jpeg;base64,${f.preview_base64}">` : `<span>${fileIcon(f)}</span>`}</div>
          <div class="card-body">
            <div class="card-name" title="${esc(f.rel_path)}">${esc(f.name)}</div>
            <div class="card-meta"><span>${esc(f.mod_time?.slice(0, 10) || '—')}</span><span>${fmtBytes(f.original_size)}</span></div>
          </div>
        </div>`).join('')}
    </div>`;
  $('peek-extract-bar').classList.remove('hidden');
  $('peek-sel-count').textContent = `Todos os ${m.files.length} arquivo(s) (ou marque os desejados)`;
}

function renderCalibrationResults(results) {
  $('cal-results').innerHTML = `
    <table class="file-table">
      <thead><tr><th>Perfil</th><th>Carga/Frame</th><th>Erros Corrigidos (BER)</th><th>Resultado</th></tr></thead>
      <tbody>
        ${results.map(r => `
          <tr>
            <td><b>${esc(r.profile_code)}</b></td>
            <td>${fmtBytes(r.payload_per_frame)}</td>
            <td>${r.byte_error_rate_pct.toFixed(3)}% (${r.corrected_bytes} B)</td>
            <td style="color:${r.passed ? 'var(--ok)' : 'var(--danger)'}">${r.passed ? '✅ 100% Íntegro' : '❌ ' + esc(r.error)}</td>
          </tr>`).join('')}
      </tbody>
    </table>`;
}

// --- EVENTS ---
function switchAuthTab(tab) {
  document.querySelectorAll('[data-auth-tab]').forEach(b => b.classList.toggle('active', b.dataset.authTab === tab));
  $('form-unlock').classList.toggle('hidden', tab !== 'unlock');
  $('form-create').classList.toggle('hidden', tab !== 'create');
  $('form-restore').classList.toggle('hidden', tab !== 'restore');
  $('auth-error').classList.add('hidden');
}

function showAuthError(msg) {
  $('auth-error').textContent = msg;
  $('auth-error').classList.remove('hidden');
}

function openModal(id) { $(id).classList.remove('hidden'); }
function closeModals() { document.querySelectorAll('.modal').forEach(m => m.classList.add('hidden')); }

function bindEvents() {
  document.querySelectorAll('[data-auth-tab]').forEach(b => b.onclick = () => switchAuthTab(b.dataset.authTab));
  document.querySelectorAll('[data-close]').forEach(b => b.onclick = () => {
    const m = b.closest('.modal');
    if (m) m.classList.add('hidden');
    else closeModals();
  });
  $('lightbox').onclick = () => $('lightbox').classList.add('hidden');

  const pickSingleKeyfile = async (inputId) => {
    try {
      const res = await api('/api/system/pick-files', { method: 'POST', body: JSON.stringify({ title: 'Escolha o Arquivo-Chave local (2º fator)' }) });
      if (res.paths && res.paths.length > 0) $(inputId).value = res.paths[0];
    } catch (err) { toast('❌ ' + err.message, 4000); }
  };
  const generateNewKeyfile = async (inputId) => {
    try {
      const res = await api('/api/vault/generate-keyfile', { method: 'POST' });
      if (res.path) {
        $(inputId).value = res.path;
        toast(`✨ Arquivo-Chave gerado em: ${res.name} — guarde uma cópia em local seguro!`, 6000);
      }
    } catch (err) { toast('❌ ' + err.message, 5000); }
  };

  $('btn-unlock-pick-kf').onclick = () => pickSingleKeyfile('unlock-keyfile-path');
  $('btn-create-pick-kf').onclick = () => pickSingleKeyfile('create-keyfile-path');
  $('btn-create-gen-kf').onclick = () => generateNewKeyfile('create-keyfile-path');
  $('btn-sec-pick-cur-kf').onclick = () => pickSingleKeyfile('sec-cur-kf-path');
  $('btn-sec-pick-new-kf').onclick = () => pickSingleKeyfile('sec-new-kf-path');
  $('btn-sec-gen-kf').onclick = () => generateNewKeyfile('sec-new-kf-path');

  $('form-unlock').onsubmit = async (e) => {
    e.preventDefault();
    try {
      await api('/api/vault/unlock', {
        method: 'POST',
        body: JSON.stringify({
          password: $('unlock-password').value,
          keyfile_path: $('unlock-keyfile-path').value.trim(),
          remember: $('unlock-remember').checked
        })
      });
      $('unlock-password').value = '';
      await refreshStatus();
    } catch (err) { showAuthError(err.message); }
  };

  $('btn-unlock-remembered').onclick = async () => {
    try {
      await api('/api/vault/unlock-remembered', { method: 'POST' });
      await refreshStatus();
    } catch (err) { showAuthError(err.message); }
  };

  $('btn-forget-saved').onclick = async () => {
    await api('/api/vault/forget-password', { method: 'POST' });
    $('remembered-box').classList.add('hidden');
    toast('Senha salva removida deste computador.');
  };

  $('form-create').onsubmit = async (e) => {
    e.preventDefault();
    const p1 = $('create-password').value, p2 = $('create-password2').value;
    if (p1 !== p2) return showAuthError('As senhas não coincidem.');
    try {
      await api('/api/vault/create', {
        method: 'POST',
        body: JSON.stringify({
          vault_name: $('create-name').value,
          password: p1,
          keyfile_path: $('create-keyfile-path').value.trim(),
          remember: $('create-remember').checked
        })
      });
      await refreshStatus();
    } catch (err) { showAuthError(err.message); }
  };

  $('form-restore').onsubmit = async (e) => {
    e.preventDefault();
    try {
      const file = $('restore-file').files[0];
      let b64 = '';
      if (file) {
        const buf = await file.arrayBuffer();
        b64 = btoa(String.fromCharCode(...new Uint8Array(buf)));
      }
      await api('/api/catalog/restore', {
        method: 'POST',
        body: JSON.stringify({ source: $('restore-source').value, base64_snapshot: b64, password: $('restore-password').value })
      });
      toast('🎉 Catálogo restaurado com sucesso!');
      await refreshStatus();
    } catch (err) { showAuthError(err.message); }
  };

  $('btn-lock').onclick = async () => {
    await api('/api/vault/lock', { method: 'POST' });
    clearInterval(state.jobsTimer);
    await refreshStatus();
  };

  $('btn-shutdown').onclick = async () => {
    if (!confirm('Encerrar o BrightVault completamente? (O cofre será bloqueado com segurança.)')) return;
    await fetch('/api/shutdown', { method: 'POST', credentials: 'same-origin' }).catch(() => { });
    document.body.innerHTML = '<div style="height:100vh;display:flex;align-items:center;justify-content:center;flex-direction:column;gap:10px;background:var(--bg);color:var(--text);font-family:Segoe UI,sans-serif"><h2>BrightVault encerrado com segurança 🔒</h2><p style="color:var(--muted)">Você já pode fechar esta janela.</p></div>';
    setTimeout(() => window.close(), 300);
  };

  // Search & filters
  let searchT;
  $('search-input').oninput = () => {
    clearTimeout(searchT);
    searchT = setTimeout(() => { state.filters.query = $('search-input').value; runSearch(); }, 180);
  };
  $('filter-media').onchange = () => { state.filters.media_type = $('filter-media').value; runSearch(); };
  $('filter-date-from').onchange = () => { state.filters.date_from = $('filter-date-from').value.trim(); runSearch(); };
  $('filter-date-to').onchange = () => { state.filters.date_to = $('filter-date-to').value.trim(); runSearch(); };
  $('sort-select').onchange = () => { state.filters.sort_by = $('sort-select').value; runSearch(); };
  $('btn-filter-nodate').onclick = () => {
    state.filters.only_no_date = !state.filters.only_no_date;
    $('btn-filter-nodate').classList.toggle('active', state.filters.only_no_date);
    runSearch();
  };

  document.querySelectorAll('[data-view]').forEach(b => b.onclick = () => {
    state.viewMode = b.dataset.view;
    document.querySelectorAll('[data-view]').forEach(x => x.classList.toggle('active', x === b));
    renderFiles();
  });

  // Sidebar delegation (folders: select, rename, delete)
  const openDeleteFolderModal = (folderId) => {
    const f = state.vault.folders.find(x => x.id === folderId);
    if (!f) return;
    $('del-folder-id').value = folderId;
    $('del-folder-desc').innerHTML = `Você está prestes a excluir a pasta <b>📁 ${esc(f.name)}</b>. Escolha o que fazer com os arquivos e subpastas dentro dela:`;
    $('del-folder-yt').checked = false;
    openModal('modal-delete-folder');
  };

  // Reusable Confirmation Popup (overlays any modal cleanly)
  let activeConfirmHandler = null;
  const closeConfirmModal = () => {
    $('modal-confirm').classList.add('hidden');
    activeConfirmHandler = null;
  };
  const openConfirmModal = ({ title, descHtml, optionsHtml = '', okLabel = 'Confirmar', okDanger = true, onConfirm }) => {
    $('confirm-title').textContent = title || '⚠️ Confirmar Ação';
    $('confirm-desc').innerHTML = descHtml || '';
    $('confirm-options').innerHTML = optionsHtml || '';
    const okBtn = $('btn-confirm-ok');
    okBtn.textContent = okLabel;
    okBtn.className = okDanger ? 'btn danger-ghost' : 'btn primary';
    okBtn.style.borderColor = okDanger ? 'var(--danger)' : '';
    activeConfirmHandler = onConfirm;
    $('modal-confirm').classList.remove('hidden');
  };
  $('btn-confirm-x').onclick = closeConfirmModal;
  $('btn-confirm-cancel').onclick = closeConfirmModal;
  $('btn-confirm-ok').onclick = async () => {
    const fn = activeConfirmHandler;
    if (!fn) return closeConfirmModal();
    $('btn-confirm-ok').disabled = true;
    try {
      await fn();
      closeConfirmModal();
    } catch (err) {
      toast('❌ ' + err.message, 5500);
    } finally {
      $('btn-confirm-ok').disabled = false;
    }
  };

  // Folder Edit / Properties / Parent / Share / Delete Modal
  const openFolderPropsModal = async (folderId) => {
    const fid = folderId || 'root';
    const isRoot = fid === 'root';
    $('fp-folder-id').value = fid;
    $('fp-share-box').classList.add('hidden');
    $('fp-share-warnings').classList.add('hidden');
    $('fp-key-input').type = 'password';
    $('btn-fp-toggle-key').textContent = '👁️ Revelar';
    $('fp-manage-section')?.classList.toggle('hidden', isRoot);
    $('fp-delete-section')?.classList.toggle('hidden', isRoot);
    if ($('fp-wrap-parent-name')) $('fp-wrap-parent-name').value = '';
    $('fp-stats').innerHTML = '⏳ Carregando propriedades e chave criptográfica...';
    openModal('modal-folder-props');
    try {
      const p = await api(`/api/folders/${fid}/props`);
      $('fp-modal-title').textContent = `✏️ Pasta: ${p.id === 'root' ? 'Raiz (Meu BrightVault)' : p.name}`;
      if ($('fp-rename-input')) $('fp-rename-input').value = p.name || '';
      $('fp-key-input').value = p.folder_key_hex || '';
      $('fp-stats').innerHTML = `
        <div><b>📁 ${esc(p.name)}</b> ${p.subfolder_count ? `· ${p.subfolder_count} subpasta(s)` : ''}</div>
        <div style="margin-top:4px">
          📄 <b>${p.total_files}</b> arquivo(s) (${fmtBytes(p.total_bytes)}) ·
          🎬 <b>${p.container_count}</b> vídeo(s) (<b>${p.yt_linked_count}</b> no YouTube${p.pending_count ? `, <span style="color:var(--warn)">${p.pending_count} pendente(s)</span>` : ''}${p.private_count ? `, <span style="color:var(--warn)">${p.private_count} privado(s)</span>` : ''})
        </div>`;
    } catch (err) {
      $('fp-stats').innerHTML = `<div class="alert error">${esc(err.message)}</div>`;
    }
  };
  if ($('btn-fp-rename-save')) {
    $('btn-fp-rename-save').onclick = async () => {
      const fid = $('fp-folder-id').value;
      const newName = $('fp-rename-input').value.trim();
      if (!fid || fid === 'root' || !newName) return toast('Informe um nome válido para a pasta.');
      try {
        await api(`/api/folders/${fid}`, { method: 'PATCH', body: JSON.stringify({ name: newName }) });
        $('fp-modal-title').textContent = `✏️ Pasta: ${newName}`;
        toast('✏️ Pasta renomeada!');
        await refreshVault();
      } catch (err) { toast('❌ ' + err.message, 5000); }
    };
  }
  if ($('btn-fp-wrap-parent')) {
    $('btn-fp-wrap-parent').onclick = async () => {
      const fid = $('fp-folder-id').value;
      const parentName = $('fp-wrap-parent-name').value.trim();
      if (!fid || fid === 'root') return;
      if (!parentName) return toast('Digite o nome da nova Pasta Mãe.');
      try {
        await api(`/api/folders/${fid}/wrap-parent`, { method: 'POST', body: JSON.stringify({ name: parentName }) });
        $('fp-wrap-parent-name').value = '';
        toast(`📂 Pasta Mãe "${parentName}" criada!`);
        await refreshVault();
      } catch (err) { toast('❌ ' + err.message, 5000); }
    };
  }
  if ($('btn-fp-delete-folder')) {
    $('btn-fp-delete-folder').onclick = () => {
      const fid = $('fp-folder-id').value;
      if (!fid || fid === 'root') return;
      closeModals();
      openDeleteFolderModal(fid);
    };
  }
  $('btn-fp-toggle-key').onclick = () => {
    const inp = $('fp-key-input');
    const showing = inp.type === 'text';
    inp.type = showing ? 'password' : 'text';
    $('btn-fp-toggle-key').textContent = showing ? '👁️ Revelar' : '🙈 Ocultar';
  };
  $('btn-fp-copy-key').onclick = async () => {
    const val = $('fp-key-input').value;
    if (!val) return;
    try { await navigator.clipboard.writeText(val); toast('📋 Chave da pasta copiada!'); }
    catch { $('fp-key-input').type = 'text'; $('fp-key-input').select(); document.execCommand('copy'); toast('📋 Chave da pasta copiada!'); }
  };
  $('btn-fp-gen-code').onclick = async () => {
    const fid = $('fp-folder-id').value || 'root';
    const btn = $('btn-fp-gen-code');
    btn.disabled = true;
    try {
      const res = await api(`/api/folders/${fid}/share-code`, {
        method: 'POST',
        body: JSON.stringify({ include_subfolders: $('fp-include-sub').checked })
      });
      $('fp-share-code').value = res.code;
      $('fp-share-meta').textContent = `✅ ${res.file_count} arquivo(s) · ${res.container_count} vídeo(s) · Snapshot gerado agora`;
      $('fp-share-box').classList.remove('hidden');
      const warnEl = $('fp-share-warnings');
      if (res.warnings && res.warnings.length) {
        warnEl.innerHTML = `<div class="hint-box" style="border-color:var(--warn);background:rgba(251,191,36,.1)">⚠️ ${res.warnings.map(esc).join('<br>⚠️ ')}</div>`;
        warnEl.classList.remove('hidden');
      } else {
        warnEl.classList.add('hidden');
      }
      toast('🔗 Código de compartilhamento da pasta gerado!');
    } catch (err) {
      toast('❌ ' + err.message, 5500);
    } finally {
      btn.disabled = false;
    }
  };
  $('btn-fp-copy-code').onclick = async () => {
    const val = $('fp-share-code').value;
    if (!val) return;
    try { await navigator.clipboard.writeText(val); toast('📋 Código BVFOLDER1 copiado!'); }
    catch { $('fp-share-code').select(); document.execCommand('copy'); toast('📋 Código BVFOLDER1 copiado!'); }
  };

  $('folder-tree').onclick = async (e) => {
    const propsBtn = e.target.closest('[data-folder-props]');
    if (propsBtn) {
      e.stopPropagation();
      openFolderPropsModal(propsBtn.dataset.folderProps);
      return;
    }
    const ren = e.target.closest('[data-folder-rename]');
    if (ren) {
      e.stopPropagation();
      const f = state.vault.folders.find(x => x.id === ren.dataset.folderRename);
      const newName = prompt('Novo nome da pasta:', f?.name || '');
      if (!newName || !newName.trim()) return;
      await api(`/api/folders/${ren.dataset.folderRename}`, { method: 'PATCH', body: JSON.stringify({ name: newName.trim() }) });
      toast('✏️ Pasta renomeada!');
      await refreshVault();
      return;
    }
    const del = e.target.closest('[data-folder-del]');
    if (del) {
      e.stopPropagation();
      openDeleteFolderModal(del.dataset.folderDel);
      return;
    }
    const b = e.target.closest('[data-folder]');
    if (!b) return;
    state.filters.folder_id = b.dataset.folder;
    state.filters.collection_id = '';
    state.filters.container_id = '';
    state.selectedIds.clear();
    renderSidebar(); runSearch();
  };

  $('btn-props-current-folder').onclick = () => {
    openFolderPropsModal(state.filters.folder_id || 'root');
  };

  // Drag-and-drop sibling folder reordering (STRICTLY within same ParentID — never pulls out of parent!)
  let dragFolderId = null;
  let dragFolderParent = null;
  const clearFolderDragStyles = () => {
    $('folder-tree').querySelectorAll('.folder-row').forEach(r => r.classList.remove('dragging', 'drag-over-top', 'drag-over-bottom'));
  };
  $('folder-tree').ondragstart = (e) => {
    const row = e.target.closest('.folder-row[draggable="true"]');
    if (!row) return;
    dragFolderId = row.dataset.folderRow;
    dragFolderParent = row.dataset.parentId || 'root';
    row.classList.add('dragging');
    e.dataTransfer.effectAllowed = 'move';
    try { e.dataTransfer.setData('text/plain', dragFolderId); } catch { }
  };
  $('folder-tree').ondragend = () => {
    dragFolderId = null;
    dragFolderParent = null;
    clearFolderDragStyles();
  };
  $('folder-tree').ondragover = (e) => {
    if (!dragFolderId) return;
    const row = e.target.closest('.folder-row[draggable="true"]');
    if (!row || row.dataset.folderRow === dragFolderId) return;
    const targetParent = row.dataset.parentId || 'root';
    if (targetParent !== dragFolderParent) return; // strictly forbid moving outside parent!
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    $('folder-tree').querySelectorAll('.drag-over-top,.drag-over-bottom').forEach(x => x.classList.remove('drag-over-top', 'drag-over-bottom'));
    const rect = row.getBoundingClientRect();
    const isTop = (e.clientY - rect.top) < rect.height / 2;
    row.classList.add(isTop ? 'drag-over-top' : 'drag-over-bottom');
  };
  $('folder-tree').ondrop = async (e) => {
    if (!dragFolderId) return;
    const row = e.target.closest('.folder-row[draggable="true"]');
    if (!row) return;
    const targetId = row.dataset.folderRow;
    const targetParent = row.dataset.parentId || 'root';
    if (!targetId || targetId === dragFolderId || targetParent !== dragFolderParent) {
      clearFolderDragStyles();
      return;
    }
    e.preventDefault();
    const rect = row.getBoundingClientRect();
    const insertBefore = (e.clientY - rect.top) < rect.height / 2;
    const siblings = [...$('folder-tree').querySelectorAll(`.folder-row[data-parent-id="${CSS.escape(dragFolderParent)}"]`)]
      .map(r => r.dataset.folderRow)
      .filter(id => id && id !== 'root');
    const withoutDragged = siblings.filter(id => id !== dragFolderId);
    let idx = withoutDragged.indexOf(targetId);
    if (idx < 0) { clearFolderDragStyles(); return; }
    if (!insertBefore) idx++;
    withoutDragged.splice(idx, 0, dragFolderId);
    clearFolderDragStyles();
    try {
      await api('/api/folders/reorder', {
        method: 'POST',
        body: JSON.stringify({ parent_id: dragFolderParent, ordered_ids: withoutDragged })
      });
      await refreshVault();
    } catch (err) { toast('❌ ' + err.message, 5000); }
  };

  if ($('btn-delete-current-folder')) {
    $('btn-delete-current-folder').onclick = () => {
      if (state.filters.folder_id && state.filters.folder_id !== 'root') {
        openDeleteFolderModal(state.filters.folder_id);
      }
    };
  }

  $('btn-confirm-delete-folder').onclick = async () => {
    const id = $('del-folder-id').value;
    const mode = document.querySelector('input[name="del-folder-mode"]:checked')?.value || 'recursive';
    const delYT = $('del-folder-yt').checked;
    try {
      const res = await api(`/api/folders/${id}?mode=${mode}&delete_yt=${delYT}`, { method: 'DELETE' });
      closeModals();
      if (state.filters.folder_id === id) state.filters.folder_id = '';
      state.activeFile = null;
      state.selectedIds.clear();
      $('inspector-content').classList.add('hidden');
      $('inspector-empty').classList.remove('hidden');
      toast(`🗑️ Pasta excluída (${res.removed_folders} pasta(s), ${res.removed_files} arquivo(s))!`);
      await refreshVault();
    } catch (err) { toast('❌ ' + err.message, 5000); }
  };
  $('collection-list').onclick = (e) => {
    const b = e.target.closest('[data-col]');
    if (!b) return;
    state.filters.collection_id = state.filters.collection_id === b.dataset.col ? '' : b.dataset.col;
    state.filters.folder_id = ''; state.filters.container_id = '';
    renderSidebar(); runSearch();
  };
  $('year-list').onclick = (e) => {
    const b = e.target.closest('[data-year]');
    if (!b) return;
    const y = b.dataset.year;
    const same = state.filters.date_from === y && state.filters.date_to === y;
    state.filters.date_from = same ? '' : y;
    state.filters.date_to = same ? '' : y;
    $('filter-date-from').value = state.filters.date_from;
    $('filter-date-to').value = state.filters.date_to;
    renderSidebar(); runSearch();
  };
  $('tag-list').onclick = (e) => {
    const b = e.target.closest('[data-tag]');
    if (!b) return;
    state.filters.tag = state.filters.tag === b.dataset.tag ? '' : b.dataset.tag;
    renderSidebar(); runSearch();
  };
  const handleContainerButtons = async (e) => {
    const fBtn = e.target.closest('[data-cont-filter]');
    if (fBtn) {
      state.filters.container_id = state.filters.container_id === fBtn.dataset.contFilter ? '' : fBtn.dataset.contFilter;
      state.filters.folder_id = ''; state.filters.collection_id = '';
      closeModals();
      renderSidebar(); runSearch(); return;
    }
    const upBtn = e.target.closest('[data-cont-upload]');
    if (upBtn) {
      try {
        await api(`/api/containers/${upBtn.dataset.contUpload}/upload-yt`, { method: 'POST' });
        $('jobs-drawer').classList.remove('hidden');
        pollJobs();
        toast('🚀 Enviando vídeo pronto para o YouTube...');
      } catch (err) {
        toast('❌ ' + err.message, 6000);
      }
      return;
    }
    const openBtn = e.target.closest('[data-cont-open]');
    if (openBtn) {
      await api(`/api/containers/${openBtn.dataset.contOpen}/open-folder`, { method: 'POST' });
      return;
    }
    const unlBtn = e.target.closest('[data-cont-unlisted]');
    if (unlBtn) {
      try {
        await api(`/api/containers/${unlBtn.dataset.contUnlisted}/set-privacy`, {
          method: 'POST',
          body: JSON.stringify({ privacy: 'unlisted' })
        });
        toast('🔓 Vídeo alterado para Não Listado no YouTube!');
        await refreshVault();
      } catch (err) { toast('❌ ' + err.message, 6000); }
      return;
    }
    const verBtn = e.target.closest('[data-cont-verify]');
    if (verBtn) {
      try {
        const res = await api(`/api/containers/${verBtn.dataset.contVerify}/verify`, { method: 'POST' });
        toast(`✅ Vídeo verificado online (${res.detected_height}p)!`);
        await refreshVault();
      } catch (err) { toast('❌ ' + err.message, 5000); }
      return;
    }
    const renBtn = e.target.closest('[data-cont-rename]');
    if (renBtn) {
      const c = state.vault.containers.find(x => x.id === renBtn.dataset.contRename);
      if (!c) return;
      const newTitle = prompt('Novo nome / título para este vídeo no YouTube e no catálogo:', c.short_code || '');
      if (newTitle === null || !newTitle.trim() || newTitle.trim() === c.short_code) return;
      try {
        const res = await api(`/api/containers/${c.id}/set-title`, {
          method: 'POST',
          body: JSON.stringify({ title: newTitle.trim() })
        });
        let msg = `✏️ Nome atualizado para "${res.title}"`;
        if (res.yt_updated) msg += ' (atualizado também no YouTube!)';
        else if (res.yt_warning) msg += ` (salvo no catálogo; aviso YT: ${res.yt_warning})`;
        toast(msg, 5500);
        await refreshVault();
      } catch (err) { toast('❌ ' + err.message, 6000); }
      return;
    }
    const lBtn = e.target.closest('[data-cont-link]');
    if (lBtn) {
      const c = state.vault.containers.find(x => x.id === lBtn.dataset.contLink);
      $('link-cont-id').value = c.id;
      $('link-yt-url').value = c.youtube_url || '';
      $('link-cont-info').innerHTML = `<b>Título sugerido no YouTube:</b> <code>${esc(c.short_code)}</code><br><b>Arquivo gerado:</b> ${esc(c.local_video_path || '(já removido)')}`;
      openModal('modal-link-yt'); return;
    }
    const exBtn = e.target.closest('[data-cont-extract]');
    if (exBtn) {
      const dest = prompt('Pasta de destino para extrair todos os arquivos deste vídeo (deixe vazio para Downloads/BrightVault-Restaurados):', '');
      if (dest === null) return;
      await api('/api/containers/extract', { method: 'POST', body: JSON.stringify({ container_id: exBtn.dataset.contExtract, dest_dir: dest, apply_edited_meta: true, skip_unchanged_local: true }) });
      $('jobs-drawer').classList.remove('hidden'); pollJobs(); return;
    }
    const delBtn = e.target.closest('[data-cont-del]');
    if (delBtn) {
      const c = state.vault.containers.find(x => x.id === delBtn.dataset.contDel);
      openConfirmModal({
        title: '🧹 Limpar .mp4 Temporário Local',
        descHtml: `Deseja apagar o arquivo local temporário de <b>${esc(c?.short_code || '')}</b> do seu HD?<br><span class="muted">Certifique-se de que o vídeo já foi enviado para o YouTube antes de apagar a cópia local.</span>`,
        okLabel: '🧹 Apagar .mp4 Local',
        onConfirm: async () => {
          await api(`/api/containers/${delBtn.dataset.contDel}/cleanup`, { method: 'POST' });
          toast('🧹 Vídeo local temporário removido.');
          await refreshVault();
        }
      });
      return;
    }
    const disBtn = e.target.closest('[data-cont-disassoc]');
    if (disBtn) {
      const c = state.vault.containers.find(x => x.id === disBtn.dataset.contDisassoc);
      if (!c) return;
      openConfirmModal({
        title: '🔌 Desassociar Arquivos do Vídeo',
        descHtml: `Deseja desvincular os <b>${c.file_count} arquivo(s)</b> associados ao vídeo <b>${esc(c.short_code)}</b>?<br><span class="muted">Os arquivos continuarão visíveis no seu catálogo com seus metadados, mas sem vínculo com este vídeo contêiner.</span>`,
        optionsHtml: `
          <label class="check-row"><input type="checkbox" id="conf-disassoc-remove-cont"> Também remover o registro deste vídeo da lista (sem apagar do YouTube)</label>`,
        okLabel: '🔌 Confirmar Desassociação',
        okDanger: false,
        onConfirm: async () => {
          const removeCont = !!$('conf-disassoc-remove-cont')?.checked;
          const res = await api(`/api/containers/${c.id}/disassociate`, {
            method: 'POST',
            body: JSON.stringify({ remove_container: removeCont })
          });
          toast(`🔌 ${res.detached_files} arquivo(s) desassociado(s)!`);
          await refreshVault();
        }
      });
      return;
    }
    const remBtn = e.target.closest('[data-cont-remove]');
    if (remBtn) {
      const c = state.vault.containers.find(x => x.id === remBtn.dataset.contRemove);
      if (!c) return;
      const hasLocal = !c.local_deleted && !!c.local_video_path;
      const hasYT = !!c.youtube_video_id;
      openConfirmModal({
        title: '🗑️ Deletar Vídeo Contêiner',
        descHtml: `Você está prestes a deletar o vídeo <b>${esc(c.short_code)}</b> (${c.file_count} arquivo(s) associado(s)) do catálogo do BrightVault.`,
        optionsHtml: `
          ${c.file_count > 0 ? `
            <div style="background:var(--bg);border:1px solid var(--border);border-radius:8px;padding:8px 10px;display:flex;flex-direction:column;gap:5px">
              <div style="font-size:11.5px;font-weight:700;color:var(--muted)">O que fazer com os ${c.file_count} arquivo(s) associados?</div>
              <label class="check-row" style="margin:0"><input type="radio" name="conf-del-filemode" value="disassociate" checked> <b>Desassociar arquivos</b> (mantém registros no catálogo sem vídeo)</label>
              <label class="check-row" style="margin:0"><input type="radio" name="conf-del-filemode" value="delete"> <b>Excluir arquivos</b> permanentemente do catálogo</label>
            </div>` : ''}
          ${hasLocal ? `<label class="check-row"><input type="checkbox" id="conf-del-local" checked> Apagar também o arquivo <code>.mp4</code> local do HD</label>` : ''}
          ${hasYT && state.vault.youtube_connected ? `<label class="check-row"><input type="checkbox" id="conf-del-yt"> Apagar também este vídeo do <b>YouTube</b> via API (irreversível)</label>` : ''}
        `,
        okLabel: '🗑️ Confirmar Exclusão do Vídeo',
        okDanger: true,
        onConfirm: async () => {
          const fileMode = document.querySelector('input[name="conf-del-filemode"]:checked')?.value || 'disassociate';
          const deleteLocal = !!$('conf-del-local')?.checked;
          const deleteYT = !!$('conf-del-yt')?.checked;
          const res = await api(`/api/containers/${c.id}/delete`, {
            method: 'POST',
            body: JSON.stringify({ file_mode: fileMode, delete_local: deleteLocal, delete_yt: deleteYT })
          });
          if (state.filters.container_id === c.id) state.filters.container_id = '';
          const parts = ['🗑️ Vídeo deletado'];
          if (res.deleted_files) parts.push(`${res.deleted_files} arquivo(s) excluído(s)`);
          if (res.detached_files) parts.push(`${res.detached_files} desassociado(s)`);
          if (res.yt_deleted) parts.push('removido do YouTube');
          if (res.yt_error) parts.push('aviso YT: ' + res.yt_error);
          toast(parts.join(' · '), 5500);
          await refreshVault();
        }
      });
    }
  };

  const triggerUploadAllPending = async (e) => {
    if (e) e.stopPropagation();
    try {
      await api('/api/containers/upload-all', { method: 'POST' });
      $('jobs-drawer').classList.remove('hidden');
      pollJobs();
      toast('🚀 Enviando todos os vídeos pendentes para o YouTube...');
    } catch (err) {
      toast('❌ ' + err.message, 6000);
    }
  };
  const openMyVideosModal = () => {
    renderMyVideosModal();
    openModal('modal-my-videos');
  };
  if ($('btn-upload-all-videos')) $('btn-upload-all-videos').onclick = triggerUploadAllPending;
  if ($('btn-mv-upload-all')) $('btn-mv-upload-all').onclick = triggerUploadAllPending;
  if ($('btn-mv-reconnect-yt')) {
    $('btn-mv-reconnect-yt').onclick = async () => {
      try {
        await api('/api/youtube/oauth/reconnect', { method: 'POST' });
        $('jobs-drawer').classList.remove('hidden');
        pollJobs();
        toast('🔄 Abrindo janela do Google para reconectar sua conta do YouTube...', 5500);
      } catch (err) { toast('❌ ' + err.message, 6000); }
    };
  }
  if ($('btn-open-my-videos')) $('btn-open-my-videos').onclick = (e) => { e.stopPropagation(); openMyVideosModal(); };
  if ($('videos-summary-card')) {
    $('videos-summary-card').onclick = (e) => {
      if (e.target.closest('#btn-upload-all-videos')) return;
      openMyVideosModal();
    };
  }
  document.querySelectorAll('[data-myvid-filter]').forEach(b => {
    b.onclick = () => {
      state.myVideosFilter = b.dataset.myvidFilter;
      document.querySelectorAll('[data-myvid-filter]').forEach(x => x.classList.toggle('active', x === b));
      renderMyVideosModal();
    };
  });
  if ($('my-videos-list')) {
    $('my-videos-list').onclick = handleContainerButtons;
    $('my-videos-list').onchange = async (e) => {
      const sel = e.target.closest('[data-cont-privacy]');
      if (!sel) return;
      const cid = sel.dataset.contPrivacy;
      const priv = sel.value;
      try {
        await api(`/api/containers/${cid}/set-privacy`, {
          method: 'POST',
          body: JSON.stringify({ privacy: priv })
        });
        const labels = { public: '🌍 Público', unlisted: '🔗 Não Listado', private: '🔒 Privado' };
        toast(`✅ Privacidade alterada para ${labels[priv] || priv} no YouTube!`);
        await refreshVault();
      } catch (err) {
        toast('❌ ' + err.message, 6000);
        renderMyVideosModal();
      }
    };
  }
  $('pending-yt-banner').onclick = handleContainerButtons;
  $('pending-dl-banner').onclick = async (e) => {
    const rBtn = e.target.closest('[data-dl-resume]');
    if (rBtn) {
      try {
        await api(`/api/downloads/${rBtn.dataset.dlResume}/resume`, { method: 'POST' });
        $('pending-dl-banner').classList.add('hidden');
        $('jobs-drawer').classList.remove('hidden');
        pollJobs();
        toast('▶️ Retomando download de onde parou!');
      } catch (err) { toast('❌ ' + err.message, 5000); }
      return;
    }
    const cBtn = e.target.closest('[data-dl-cancel]');
    if (cBtn) {
      if (!confirm('Descartar este download interrompido e apagar o arquivo parcial?')) return;
      await api(`/api/downloads/${cBtn.dataset.dlCancel}`, { method: 'DELETE' });
      toast('Download interrompido descartado.');
      await refreshVault();
    }
  };

  $('btn-new-folder').onclick = async () => {
    const name = prompt('Nome da nova pasta:');
    if (!name) return;
    await api('/api/folders', { method: 'POST', body: JSON.stringify({ parent_id: state.filters.folder_id || 'root', name }) });
    await refreshVault();
  };
  $('btn-new-collection').onclick = async () => {
    const name = prompt('Nome da nova coleção (ex: Viagem 1998, Família):');
    if (!name) return;
    await api('/api/collections', { method: 'POST', body: JSON.stringify({ name }) });
    await refreshVault();
  };

  // File multi-selection (checkbox, Shift+Click range, Ctrl+Click toggle, Select All, Ctrl+A)
  const toggleSelectRange = (targetId) => {
    const ids = state.files.map(f => f.id);
    const a = ids.indexOf(state.lastSelectedId);
    const b = ids.indexOf(targetId);
    if (a !== -1 && b !== -1) {
      const [lo, hi] = a < b ? [a, b] : [b, a];
      for (let i = lo; i <= hi; i++) state.selectedIds.add(ids[i]);
    } else {
      state.selectedIds.add(targetId);
    }
    state.lastSelectedId = targetId;
  };

  $('btn-select-all').onclick = () => {
    if (!state.files.length) return;
    if (state.selectedIds.size === state.files.length) {
      state.selectedIds.clear();
    } else {
      state.files.forEach(f => state.selectedIds.add(f.id));
    }
    renderFiles();
  };

  document.addEventListener('keydown', (e) => {
    if (!state.status?.vault_unlocked) return;
    const tag = document.activeElement?.tagName;
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'a') {
      e.preventDefault();
      state.files.forEach(f => state.selectedIds.add(f.id));
      renderFiles();
    } else if (e.key === 'Delete' && state.selectedIds.size > 0) {
      $('btn-batch-delete').click();
    }
  });

  $('files-viewport').onclick = (e) => {
    const chk = e.target.closest('[data-chk-id]');
    if (chk) {
      const id = chk.dataset.chkId;
      if (e.shiftKey && state.lastSelectedId) {
        toggleSelectRange(id);
        renderFiles();
      } else {
        if (chk.checked) state.selectedIds.add(id); else state.selectedIds.delete(id);
        state.lastSelectedId = id;
        updateBatchBar();
      }
      return;
    }
    const row = e.target.closest('[data-file-id]');
    if (!row) return;
    const id = row.dataset.fileId;
    if (e.shiftKey && state.lastSelectedId) {
      toggleSelectRange(id);
      renderFiles();
      return;
    }
    if (e.ctrlKey || e.metaKey) {
      if (state.selectedIds.has(id)) state.selectedIds.delete(id); else state.selectedIds.add(id);
      state.lastSelectedId = id;
      renderFiles();
      return;
    }
    // Single click on already-active file closes the inspector panel!
    if (state.activeFile?.id === id) {
      closeInspector();
      return;
    }
    const f = state.files.find(x => x.id === id);
    if (f) { state.lastSelectedId = id; openInspector(f); renderFiles(); }
  };

  // Native Windows Open / Open With / Post-Download Folder & Container actions
  const triggerFileAction = async (fileId, mode) => {
    if (!fileId) return;
    try {
      const res = await api(`/api/files/${fileId}/open`, {
        method: 'POST',
        body: JSON.stringify({ mode })
      });
      if (res.extracting) {
        $('jobs-drawer').classList.remove('hidden');
        pollJobs();
        toast(mode === 'open_with' ? '⏳ Extraindo para abrir com "Abrir com..." no Windows...' : '⏳ Extraindo arquivo para abrir no Windows...', 4500);
      } else if (res.opened) {
        toast(mode === 'open_with' ? '🔧 Abrindo seletor "Abrir com..." do Windows!' : '📂 Arquivo aberto no aplicativo padrão do Windows!');
      } else if (mode === 'reveal_local') {
        toast('📁 Pasta local aberta no Explorer!');
      } else if (mode === 'reveal_origin') {
        toast('🗂️ Pasta de origem aberta no Explorer!');
      } else if (mode === 'open_container') {
        toast('🎬 Contêiner aberto!');
      }
    } catch (err) {
      toast('❌ ' + err.message, 5500);
    }
  };

  // Custom Context Menu (Right-click on "Abrir" button OR on any file card/row!)
  const hideContextMenu = () => $('file-context-menu')?.classList.add('hidden');
  const showFileContextMenu = (ev, file) => {
    if (!file) return;
    ev.preventDefault();
    ev.stopPropagation();
    const menu = $('file-context-menu');
    if (!menu) return;
    const hasLocalCopy = !!(file.last_extracted_path || (file.local_source_path && !file.local_freed));
    const items = [
      { ico: '📂', label: 'Abrir (aplicativo padrão)', action: () => triggerFileAction(file.id, 'open') },
      { ico: '🔧', label: 'Abrir com...', action: () => triggerFileAction(file.id, 'open_with') },
      { ico: '⬇️', label: 'Baixar / Extrair', action: async () => {
        const dest = prompt('Pasta de destino (vazio = Downloads/BrightVault-Restaurados):', '');
        if (dest === null) return;
        await api('/api/containers/extract', { method: 'POST', body: JSON.stringify({ file_ids: [file.id], dest_dir: dest, apply_edited_meta: true }) });
        $('jobs-drawer').classList.remove('hidden'); pollJobs();
      }},
      { ico: '✏️', label: 'Editar Metadados', action: () => openMetadataModal([file.id]) },
    ];
    if (hasLocalCopy || file.local_source_path || file.container_id) {
      items.push({ sep: true });
      if (hasLocalCopy) items.push({ ico: '📁', label: 'Abrir pasta local', action: () => triggerFileAction(file.id, 'reveal_local') });
      if (file.local_source_path) items.push({ ico: '🗂️', label: 'Abrir pasta de origem', action: () => triggerFileAction(file.id, 'reveal_origin') });
      if (file.container_id) items.push({ ico: '🎬', label: 'Abrir contêiner', action: () => triggerFileAction(file.id, 'open_container') });
    }
    menu.innerHTML = '';
    items.forEach(it => {
      if (it.sep) {
        const d = document.createElement('div');
        d.className = 'ctx-sep';
        menu.appendChild(d);
        return;
      }
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'ctx-item';
      b.textContent = `${it.ico} ${it.label}`;
      b.onclick = () => { hideContextMenu(); it.action(); };
      menu.appendChild(b);
    });
    menu.classList.remove('hidden');
    const mw = menu.offsetWidth || 220;
    const mh = menu.offsetHeight || 220;
    const x = Math.min(ev.clientX, window.innerWidth - mw - 10);
    const y = Math.min(ev.clientY, window.innerHeight - mh - 10);
    menu.style.left = `${Math.max(8, x)}px`;
    menu.style.top = `${Math.max(8, y)}px`;
  };
  document.addEventListener('click', (e) => {
    if (!e.target.closest('#file-context-menu')) hideContextMenu();
  });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') hideContextMenu(); });

  $('files-viewport').oncontextmenu = (e) => {
    const row = e.target.closest('[data-file-id]');
    if (!row) return;
    const f = state.files.find(x => x.id === row.dataset.fileId);
    if (f) showFileContextMenu(e, f);
  };

  if ($('btn-close-inspector')) $('btn-close-inspector').onclick = closeInspector;
  if ($('btn-open-one')) {
    $('btn-open-one').onclick = () => { if (state.activeFile) triggerFileAction(state.activeFile.id, 'open'); };
    $('btn-open-one').oncontextmenu = (e) => { if (state.activeFile) showFileContextMenu(e, state.activeFile); };
  }
  if ($('btn-reveal-local')) $('btn-reveal-local').onclick = () => { if (state.activeFile) triggerFileAction(state.activeFile.id, 'reveal_local'); };
  if ($('btn-reveal-origin')) $('btn-reveal-origin').onclick = () => { if (state.activeFile) triggerFileAction(state.activeFile.id, 'reveal_origin'); };
  if ($('btn-open-container')) $('btn-open-container').onclick = () => { if (state.activeFile) triggerFileAction(state.activeFile.id, 'open_container'); };
  if ($('jobs-list')) {
    $('jobs-list').onclick = async (e) => {
      const jf = e.target.closest('[data-job-file]');
      if (jf) {
        triggerFileAction(jf.dataset.jobFile, jf.dataset.jobMode || 'open');
        return;
      }
      const co = e.target.closest('[data-cont-open]');
      if (co) {
        await api(`/api/containers/${co.dataset.contOpen}/open-folder`, { method: 'POST' });
      }
    };
  }

  $('btn-extract-one').onclick = async () => {
    if (!state.activeFile) return;
    const dest = prompt('Pasta de destino (vazio = Downloads/BrightVault-Restaurados):', '');
    if (dest === null) return;
    await api('/api/containers/extract', {
      method: 'POST',
      body: JSON.stringify({ file_ids: [state.activeFile.id], dest_dir: dest, apply_edited_meta: $('chk-apply-exif').checked })
    });
    $('jobs-drawer').classList.remove('hidden'); pollJobs();
  };

  $('btn-delete-one').onclick = async () => {
    if (!state.activeFile) return;
    if (!confirm(`Excluir "${state.activeFile.display_name}" do BrightVault? (Se for o último arquivo do vídeo contêiner, o vídeo temporário local também será removido.)`)) return;
    const delYT = state.vault.youtube_connected && confirm('Se o vídeo no YouTube ficar vazio após excluir este arquivo, deseja apagá-lo do YouTube também?');
    await api(`/api/files/${state.activeFile.id}?delete_yt=${delYT}`, { method: 'DELETE' });
    state.selectedIds.delete(state.activeFile.id);
    state.activeFile = null;
    $('inspector-content').classList.add('hidden');
    $('inspector-empty').classList.remove('hidden');
    toast('🗑️ Arquivo excluído!');
    await refreshVault();
  };

  // Single-file or Batch Metadata Modal (works on single-clicked file OR multi-selected files!)
  let metaEditTargetIds = [];
  const openMetadataModal = (ids) => {
    if (!ids || !ids.length) return;
    metaEditTargetIds = [...ids];
    const isSingle = ids.length === 1;
    const singleFile = isSingle ? (state.files.find(x => x.id === ids[0]) || (state.activeFile?.id === ids[0] ? state.activeFile : null)) : null;

    $('modal-batch-title').textContent = isSingle ? '✏️ Editar Metadados do Arquivo' : `✏️ Edição de Metadados em Lote (${ids.length} arquivos)`;
    $('modal-batch-desc').textContent = isSingle
      ? 'Altere nome, data, local, pasta, tags, coleções ou notas sem precisar re-enviar o vídeo.'
      : 'Deixe em branco os campos que não deseja alterar. Ideal para organizar pastas de fotos antigas!';
    $('batch-single-name-wrap').classList.toggle('hidden', !isSingle);
    $('batch-single-extra-wrap').classList.toggle('hidden', !isSingle);
    $('batch-shift-wrap').classList.toggle('hidden', isSingle);
    $('batch-col-single-select-wrap').classList.toggle('hidden', isSingle);
    $('batch-tags-label').textContent = isSingle ? 'Tags (separadas por vírgula)' : 'Adicionar Tags (vírgula)';
    $('btn-apply-batch').textContent = isSingle ? '💾 Salvar Metadados' : `Aplicar a ${ids.length} Arquivo(s)`;

    if (isSingle && singleFile) {
      $('batch-name').value = singleFile.display_name || '';
      $('batch-date').value = singleFile.effective_date || '';
      $('batch-precision').value = singleFile.date_precision || 'exact';
      $('batch-shift-days').value = '';
      $('batch-location').value = singleFile.location_label || '';
      $('batch-folder').value = singleFile.folder_id || 'root';
      $('batch-tags').value = (singleFile.tags || []).join(', ');
      $('batch-notes').value = singleFile.notes || '';
      const colSet = new Set(singleFile.collection_ids || []);
      $('batch-collections-checks').innerHTML = state.vault.collections.length
        ? state.vault.collections.map(c => `<label class="pill"><input type="checkbox" value="${c.id}" ${colSet.has(c.id) ? 'checked' : ''}> ${esc(c.name)}</label>`).join('')
        : '<span class="muted sm-txt">Crie coleções na barra lateral</span>';
    } else {
      $('batch-name').value = '';
      $('batch-date').value = '';
      $('batch-precision').value = '';
      $('batch-shift-days').value = '';
      $('batch-location').value = '';
      $('batch-folder').value = '';
      $('batch-collection').value = '';
      $('batch-tags').value = '';
      $('batch-notes').value = '';
    }
    openModal('modal-batch');
  };

  if ($('btn-edit-meta-one')) {
    $('btn-edit-meta-one').onclick = () => {
      if (state.activeFile) openMetadataModal([state.activeFile.id]);
    };
  }

  // Batch / Active-file toolbar actions
  $('btn-batch-clear').onclick = () => {
    state.selectedIds.clear();
    state.activeFile = null;
    $('inspector-content').classList.add('hidden');
    $('inspector-empty').classList.remove('hidden');
    renderFiles();
  };
  $('btn-batch-delete').onclick = async () => {
    const ids = getTargetFileIds();
    const n = ids.length;
    if (!n) return;
    if (!confirm(`Excluir permanentemente ${n === 1 ? 'este arquivo' : `os ${n} arquivo(s) selecionado(s)`} do BrightVault?`)) return;
    const delYT = state.vault.youtube_connected && confirm('Se algum vídeo no YouTube ficar completamente vazio, deseja apagá-lo do YouTube também?');
    const res = await api('/api/files/delete-batch', {
      method: 'POST',
      body: JSON.stringify({ file_ids: ids, delete_empty_containers: true, delete_yt: delYT })
    });
    state.selectedIds.clear();
    state.activeFile = null;
    $('inspector-content').classList.add('hidden');
    $('inspector-empty').classList.remove('hidden');
    toast(`🗑️ ${res.removed} arquivo(s) excluído(s)${res.yt_deleted ? ` (${res.yt_deleted} vídeo(s) removido(s) do YouTube)` : ''}!`);
    await refreshVault();
  };
  $('btn-batch-meta').onclick = () => openMetadataModal(getTargetFileIds());
  $('btn-apply-batch').onclick = async () => {
    if (!metaEditTargetIds.length) return;
    if (metaEditTargetIds.length === 1) {
      const fid = metaEditTargetIds[0];
      const cols = [...$('batch-collections-checks').querySelectorAll('input:checked')].map(i => i.value);
      const tags = $('batch-tags').value.split(',').map(s => s.trim()).filter(Boolean);
      const updated = await api(`/api/files/${fid}`, {
        method: 'PATCH',
        body: JSON.stringify({
          display_name: $('batch-name').value,
          effective_date: $('batch-date').value,
          date_precision: $('batch-precision').value || 'exact',
          location_label: $('batch-location').value,
          folder_id: $('batch-folder').value || 'root',
          tags,
          collection_ids: cols,
          notes: $('batch-notes').value
        })
      });
      closeModals();
      toast('💾 Metadados salvos no catálogo!');
      await refreshVault();
      if (state.activeFile?.id === fid) openInspector(updated);
      return;
    }
    const payload = { file_ids: metaEditTargetIds };
    if ($('batch-date').value.trim()) payload.effective_date = $('batch-date').value.trim();
    if ($('batch-precision').value) payload.date_precision = $('batch-precision').value;
    if ($('batch-shift-days').value) payload.shift_days = parseInt($('batch-shift-days').value, 10) || 0;
    if ($('batch-location').value.trim()) payload.location_label = $('batch-location').value.trim();
    if ($('batch-folder').value) payload.move_to_folder_id = $('batch-folder').value;
    if ($('batch-collection').value) payload.add_collection = $('batch-collection').value;
    if ($('batch-tags').value.trim()) payload.add_tags = $('batch-tags').value.split(',').map(s => s.trim()).filter(Boolean);
    const res = await api('/api/files/batch-metadata', { method: 'POST', body: JSON.stringify(payload) });
    closeModals();
    toast(`✅ Metadados atualizados em ${res.updated} arquivo(s)!`);
    await refreshVault();
  };
  $('btn-batch-extract').onclick = async () => {
    const ids = getTargetFileIds();
    if (!ids.length) return;
    const dest = prompt('Pasta de destino (vazio = Downloads/BrightVault-Restaurados):', '');
    if (dest === null) return;
    await api('/api/containers/extract', {
      method: 'POST',
      body: JSON.stringify({ file_ids: ids, dest_dir: dest, apply_edited_meta: true, skip_unchanged_local: true })
    });
    $('jobs-drawer').classList.remove('hidden'); pollJobs();
  };
  $('btn-batch-free').onclick = async () => {
    const ids = getTargetFileIds();
    if (!ids.length) return;
    if (!confirm(`Remover do HD os arquivos originais dos ${ids.length} item(ns)? (Apenas arquivos já enviados/verificados no YouTube serão removidos; miniaturas e metadados continuam no BrightVault.)`)) return;
    const res = await api('/api/files/free-local', { method: 'POST', body: JSON.stringify({ file_ids: ids, confirm: true }) });
    toast(`🧹 Liberados ${fmtBytes(res.freed_bytes)} (${res.freed_files} arquivo(s))!`);
    await refreshVault();
  };

  // Upload modal & Drag/Drop — 2 clean tabs (create=Dados Brutos, artistic=Vídeo Artístico)
  state.uploadTab = 'create';
  state.selectedPayloadBytes = 0;
  state.mediaDurCache = {};
  state.lastPreviewURL = '';
  let mediaProbeTimer = null;
  let pathStatsTimer = null;

  const setUploadTab = (tab) => {
    state.uploadTab = (tab === 'artistic' || tab === 'arts' || tab === 'video') ? 'artistic' : 'create';
    document.querySelectorAll('[data-up-tab]').forEach(x => x.classList.toggle('active', x.dataset.upTab === state.uploadTab));
    $('up-tab-create').classList.toggle('hidden', state.uploadTab !== 'create');
    $('up-tab-carrier').classList.toggle('hidden', state.uploadTab === 'create');
    if (state.uploadTab === 'artistic') {
      refreshCarrierUI();
    }
  };

  const fmtDurationShort = (sec) => {
    sec = Math.max(0, Math.round(sec));
    const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
    if (h > 0) return `${h}h ${String(m).padStart(2, '0')}m ${String(s).padStart(2, '0')}s`;
    if (m > 0) return `${m}m ${String(s).padStart(2, '0')}s`;
    return `${s}s`;
  };

  const themeNamesMap = {
    none: '🚫 Nenhum (Sem Efeito Artístico)',
    'deepdream-l2': '🎨 DeepDream Layer 2 (Ondas Starry Night)',
    'deepdream-l4': '👁️ DeepDream Layer 4 (Orbs & Caleidoscópio Zoom)',
    mandelbrot: '🌌 Mandelbrot Seahorse Zoom (Coni63)',
    'julia-turing': '🧬 Julia + Bio-Turing Ultra-Macro',
    fractal: '☁️ Sonho Fractal (Nuvens & Aquarela)',
    waves: '🎵 Ondas Musicais (Osciloscópio & Espectro)',
    lightning: '⚡ Raios & Energia (Arcos Neon)',
    vortex: '🌀 Túnel Hipnótico (Espiral Radial)',
    beach: '🏖️ Praia & Oceano (Ondas & Pôr-do-Sol)',
    flora: '🌿 Flora & Natureza (Folhas & Espirais)',
    aurora: '🌌 Aurora & Plasma',
    auto: '🎲 Automático'
  };

  const getPreviewQueryParams = () => {
    const prof = $('up-carrier-profile')?.value || 'art-fullres';
    const theme = $('up-art-theme')?.value || 'deepdream-l2';
    const intensity = $('up-mask-intensity')?.value || 'balanced';
    const hasHost = ($('up-host-video')?.value.trim() || '') !== '';
    const isStealth = prof.startsWith('stealth-');
    const useHostPreview = hasHost || prof === 'video-fractal' || prof === 'video-chroma' || (isStealth && theme === 'none');
    const canHide = prof !== 'art-chroma' && (prof !== 'video-bw' || useHostPreview);
    const hideHdr = canHide && !!$('up-hide-header')?.checked;
    const tKey = (theme && theme !== 'auto' && themeNamesMap[theme]) ? theme : (theme === 'none' ? 'none' : 'deepdream-l2');
    return { prof, theme, tKey, intensity, hasHost, useHostPreview, canHide, hideHdr };
  };

  const updateStylePreviewGIF = () => {
    const img = $('up-style-gif');
    if (!img) return;
    const { prof, theme, tKey, intensity, hasHost, useHostPreview, hideHdr } = getPreviewQueryParams();
    const intLabels = {
      subtle: 'Sutil (0,25×)',
      balanced: 'Equilibrada (1×)',
      strong: 'Forte (2×)',
      max: 'Máxima (4×)'
    };
    const hdrLabel = hideHdr ? 'Tela cheia sem barras' : 'Com barra P&B no topo';
    const themeLbl = themeNamesMap[theme] || themeNamesMap['deepdream-l2'];
    const hostLbl = hasHost ? 'Vídeo Base' : (useHostPreview ? 'Vídeo Base (ex.: Steamboat Willie)' : 'Sem Vídeo Base');

    const patLabels = {
      'stealth-stars': '✨ Furtivo: Máscara de Estrelas',
      'stealth-rain': '🌧️ Furtivo: Chuva em Tela Inteira',
      'stealth-vhs': '📼 Furtivo: Fita VHS Analógica',
      'stealth-film': '🎬 Furtivo: Cinema 35mm',
      'art-fullres': '🌌 Relevo Nítido Full-Res 2-bit',
      'art-chroma': '🌈 Cromático + 6 Tons',
      'art-crystal': '💎 Cristal 3D (4 Ângulos DCT1)',
      'art-fractal3d': '☁️ Relevo 3D em Blocos 2-bit',
      'art-angles': '📐 Linhas & Ângulos (4 Direções)',
      'art-fractal-angles': '🧬 Gravura Linhas & Ângulos (Cega)',
      'art-bauhaus': '🔷 Vitral Bauhaus (Triângulos)',
      'art-constel': '✨ Constelação / Bokeh (Pontos)',
      'art-truchet': '🌀 Curvas de Truchet (Labirinto)',
      'art-stego': '🪶 Relevo Suave ±12 / Manchester',
      'video-fractal': '🎨 Croma Suave ±16 (Decod. Cega)',
      'video-chroma': '🌈 Colorização 4 Cores (Decod. Cega)',
      'video-bw': '🏁 Ruído P&B Clássico'
    };

    const parts = [patLabels[prof] || prof];
    if (theme !== 'none') parts.push(themeLbl);
    if (useHostPreview) parts.push(hasHost ? '🎬 Vídeo Base' : '🎬 Exemplo Vídeo Base');
    const title = parts.join(' · ');
    const cap = `Dados: ${intLabels[intensity] || 'Equilibrada'} · ${hdrLabel} · ${hostLbl} · Clique para expandir em HD!`;

    const baseUrl = `/api/preview/gif?profile=${encodeURIComponent(prof)}&theme=${encodeURIComponent(tKey)}&intensity=${encodeURIComponent(intensity)}&hide_header=${hideHdr ? '1' : '0'}&use_host=${useHostPreview ? '1' : '0'}`;
    state.lastPreviewURL = baseUrl;
    if (!img.src.endsWith(baseUrl)) {
      img.style.opacity = '0.55';
      img.onload = () => { img.style.opacity = '1'; };
      img.src = baseUrl;
    }
    $('up-style-gif-title').textContent = title;
    $('up-style-gif-caption').textContent = cap;

    // Live combination summary pill right under Padrão de Dados
    const comboEl = $('up-combo-hint');
    if (comboEl) {
      const noArt = theme === 'none';
      if (hasHost && (prof === 'art-fullres' || prof === 'art-fractal3d')) {
        comboEl.innerHTML = '⚠️ <b>Nota:</b> este padrão subtrativo não exibe o brilho do Vídeo Base. Para ver o Vídeo Base nítido, prefira <b>Croma Suave ±16</b>, <b>Gravura Linhas/Ângulos</b>, <b>Relevo Suave</b>, <b>4 Cores</b> ou <b>Ruído P&amp;B</b>.';
      } else if (hasHost && noArt) {
        comboEl.innerHTML = '✅ <b>Combinação ativa:</b> apenas <b>Vídeo Base</b> (sem efeito artístico) + padrão de dados selecionado em decodificação 100% cega.';
      } else if (hasHost && !noArt) {
        comboEl.innerHTML = `✅ <b>Combinação ativa:</b> <b>Vídeo Base</b> + <b>${esc(themeLbl)}</b> + padrão de dados selecionado em tela cheia limpa.`;
      } else if (!hasHost && noArt) {
        comboEl.innerHTML = '✅ <b>Combinação ativa:</b> apenas <b>Padrão de Dados geométrico/P&amp;B</b> (sem efeito artístico e sem vídeo base).';
      } else {
        comboEl.innerHTML = `✅ <b>Combinação ativa:</b> <b>${esc(themeLbl)}</b> (sem vídeo base) + padrão de dados em tela cheia limpa.`;
      }
    }
  };

  const openExpandedPreview = () => {
    const ov = $('modal-preview-expand');
    const bigImg = $('preview-expand-img');
    if (!ov || !bigImg) return;
    const baseUrl = state.lastPreviewURL || '/api/preview/gif?profile=art-fullres&theme=deepdream-l2&intensity=balanced&hide_header=1&use_host=0';
    const hdUrl = baseUrl + '&size=large';
    $('preview-expand-title').textContent = '🔍 ' + ($('up-style-gif-title')?.textContent || 'Prévia em Alta Definição');
    $('preview-expand-caption').textContent = ($('up-style-gif-caption')?.textContent || '').replace(' · Clique para expandir em HD!', '') + ' · Renderizado em HD (640×360) direto do codec real do BrightVault.';
    bigImg.style.opacity = '0.5';
    bigImg.onload = () => { bigImg.style.opacity = '1'; };
    bigImg.src = hdUrl;
    ov.classList.remove('hidden');
  };
  const closeExpandedPreview = () => {
    $('modal-preview-expand')?.classList.add('hidden');
  };
  if ($('btn-expand-preview')) $('btn-expand-preview').onclick = openExpandedPreview;
  if ($('btn-expand-preview-img')) $('btn-expand-preview-img').onclick = openExpandedPreview;
  if ($('btn-close-preview-expand')) $('btn-close-preview-expand').onclick = closeExpandedPreview;
  if ($('modal-preview-expand')) {
    $('modal-preview-expand').onclick = (e) => {
      if (e.target === $('modal-preview-expand')) closeExpandedPreview();
    };
  }
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && $('modal-preview-expand') && !$('modal-preview-expand').classList.contains('hidden')) {
      e.stopPropagation();
      closeExpandedPreview();
    }
  });

  const getCurrentCarrierCapPerFrame = () => {
    const res = $('up-carrier-res').value || '1080p';
    const prof = $('up-carrier-profile').value || 'art-stego';
    const miCode = $('up-mask-intensity')?.value || 'balanced';
    const miNum = miCode === 'subtle' ? 1 : miCode === 'strong' ? 2 : miCode === 'max' ? 3 : 0;
    const hideHdr = ($('up-hide-header')?.checked && prof !== 'art-chroma') ? 1 : 0;
    const hasHost = ($('up-host-video')?.value.trim() || '') !== '';
    const keyBase = `${miNum}_${hideHdr}`;
    const det = state.status?.frame_capacities_detailed?.[res]?.[prof];
    if (det) {
      if (hasHost && det[keyBase + '_host']) return det[keyBase + '_host'];
      if (det[keyBase]) return det[keyBase];
    }
    return state.status?.frame_capacities?.[res]?.[prof] || 1253;
  };

  const renderCapacityUI = (durInfo, loadingMsg = '', warnMsg = '') => {
    const durSrc = $('up-duration-source').value;
    const box = $('up-capacity-box');
    const btnReprobe = $('btn-reprobe-cap');
    const res = $('up-carrier-res').value || '1080p';
    const prof = $('up-carrier-profile').value || 'art-stego';
    const fps = Number($('up-carrier-fps').value) || 15;
    const capPerFrame = getCurrentCarrierCapPerFrame();
    const usedBytes = state.selectedPayloadBytes || 0;

    if (durSrc === 'files') {
      btnReprobe.classList.add('hidden');
      if (usedBytes === 0 && !durInfo) {
        box.classList.add('hidden');
        return;
      }
      box.classList.remove('hidden');
      const neededFrames = Math.max(2, Math.ceil(usedBytes / Math.max(1, capPerFrame)) + 1);
      const neededSec = neededFrames / fps;
      const m = computeComboMetrics(res, prof, fps, $('up-carrier-vcodec')?.value || 'auto', $('up-mask-intensity')?.value || 'balanced', !!$('up-hide-header')?.checked);
      const estVidBytes = usedBytes * m.estUp;

      $('up-cap-header').innerHTML = `📦 Duração calculada p/ seus arquivos (<b>${fmtBytes(usedBytes)}</b>): <b>${fmtDurationShort(neededSec)}</b> (${neededFrames.toLocaleString('pt-BR')} frames @ ${fps} FPS)`;
      if (durInfo && durInfo.duration_sec > 0) {
        const pct = (neededSec / durInfo.duration_sec) * 100;
        const barW = Math.min(100, Math.max(2, pct));
        const barColor = pct > 100 ? '#f59e0b' : '#22c55e';
        $('up-cap-pct').textContent = pct > 100 ? `${pct.toFixed(0)}% (Loop)` : `${pct.toFixed(pct < 10 ? 1 : 0)}% (Cortado)`;
        $('up-cap-pct').style.color = barColor;
        $('up-cap-bar').style.width = `${barW}%`;
        $('up-cap-bar').style.background = barColor;
        if (neededSec <= durInfo.duration_sec) {
          $('up-cap-detail').innerHTML = `✂️ O vídeo portador (<b>${fmtDurationShort(durInfo.duration_sec)}</b>) será <b>cortado exatamente em ${fmtDurationShort(neededSec)}</b> · MP4 estimado: <b>~${fmtBytes(estVidBytes)}</b>.`;
        } else {
          $('up-cap-detail').innerHTML = `🔁 Seus arquivos (<b>${fmtBytes(usedBytes)}</b>) precisam de <b>${fmtDurationShort(neededSec)}</b> — maior que o vídeo portador (<b>${fmtDurationShort(durInfo.duration_sec)}</b>), então o vídeo entrará em loop · MP4 estimado: <b>~${fmtBytes(estVidBytes)}</b>. <i>(Para um vídeo menor, aumente Resolução/FPS ou use Intensidade Forte / Arte Cromática.)</i>`;
        }
      } else {
        $('up-cap-pct').textContent = `~${fmtBytes(estVidBytes)}`;
        $('up-cap-pct').style.color = '#38bdf8';
        $('up-cap-bar').style.width = '100%';
        $('up-cap-bar').style.background = '#38bdf8';
        $('up-cap-detail').innerHTML = `🎬 O vídeo gerado terá exatamente <b>${fmtDurationShort(neededSec)}</b> para caber <b>${fmtBytes(usedBytes)}</b> (capacidade: <b>${fmtBytes(capPerFrame)}/frame</b>).`;
      }
      return;
    }
    box.classList.remove('hidden');
    btnReprobe.classList.remove('hidden');

    if (loadingMsg) {
      $('up-cap-header').textContent = '⏳ ' + loadingMsg;
      $('up-cap-pct').textContent = '...';
      $('up-cap-bar').style.width = '100%';
      $('up-cap-bar').style.background = '#38bdf8';
      $('up-cap-detail').textContent = 'Consultando duração real da mídia (sem baixar o arquivo inteiro)...';
      return;
    }
    if (warnMsg || !durInfo) {
      $('up-cap-header').textContent = 'ℹ️ Aguardando mídia mandatória';
      $('up-cap-pct').textContent = '—';
      $('up-cap-bar').style.width = '0%';
      $('up-cap-detail').textContent = warnMsg || 'Informe o vídeo ou áudio acima para calcular quantos GBs cabem no vídeo gerado.';
      return;
    }

    const totalFrames = Math.max(1, Math.round(durInfo.duration_sec * fps));
    const usableBytes = Math.max(0, totalFrames - 1) * capPerFrame;
    const pct = usableBytes > 0 ? (usedBytes / usableBytes) * 100 : 0;
    const barW = Math.min(100, Math.max(pct > 0 ? 1.5 : 0, pct));
    const barColor = pct > 100 ? '#ef4444' : pct >= 80 ? '#f59e0b' : '#22c55e';
    const capGB = usableBytes / (1024 * 1024 * 1024);
    const capLabel = capGB >= 0.1 ? `${capGB.toFixed(2)} GB (${fmtBytes(usableBytes)})` : `${fmtBytes(usableBytes)} (${capGB.toFixed(4)} GB)`;

    $('up-cap-header').innerHTML = `📊 Capacidade útil: <b>${capLabel}</b> · ${fmtDurationShort(durInfo.duration_sec)} (${totalFrames.toLocaleString('pt-BR')} frames @ ${fps} FPS)`;
    $('up-cap-pct').textContent = `${pct.toFixed(pct < 10 ? 1 : 0)}% preenchido`;
    $('up-cap-pct').style.color = barColor;
    $('up-cap-bar').style.width = `${barW}%`;
    $('up-cap-bar').style.background = barColor;

    const titleTxt = durInfo.title ? `<b>${esc(durInfo.title)}</b> · ` : '';
    if (usedBytes === 0) {
      $('up-cap-detail').innerHTML = `${titleTxt}Selecione arquivos/pasta no topo para ver a porcentagem preenchida. <i>Não é obrigatório encher 100% — o restante do vídeo/música continua tocando normalmente!</i>`;
    } else if (pct > 100) {
      $('up-cap-detail').innerHTML = `${titleTxt}⚠️ Arquivos selecionados (<b>${fmtBytes(usedBytes)}</b>) excedem a duração em <b>+${(pct - 100).toFixed(0)}%</b> — o vídeo será estendido em loop para caber tudo (ou aumente Resolução/FPS).`;
    } else {
      $('up-cap-detail').innerHTML = `${titleTxt}✅ Usando <b>${fmtBytes(usedBytes)}</b> de <b>${fmtBytes(usableBytes)}</b> (sobram <b>${fmtBytes(Math.max(0, usableBytes - usedBytes))}</b> livres — não precisa encher tudo!).`;
    }
  };

  const refreshCapacityMeter = async (forceProbe = false) => {
    const durSrc = $('up-duration-source').value;
    if (durSrc === 'files') {
      const hostSrc = $('up-host-video').value.trim();
      if (hostSrc && state.mediaDurCache[hostSrc]) {
        renderCapacityUI(state.mediaDurCache[hostSrc]);
        return;
      }
      renderCapacityUI(null);
      if (hostSrc && (state.selectedPayloadBytes || 0) > 0) {
        try {
          const info = await api('/api/media/probe-duration', { method: 'POST', body: JSON.stringify({ source: hostSrc }) });
          state.mediaDurCache[hostSrc] = info;
          if ($('up-duration-source').value === 'files') renderCapacityUI(info);
        } catch { }
      }
      return;
    }
    let src = '';
    if (durSrc === 'video') {
      src = $('up-host-video').value.trim();
      if (!src) {
        renderCapacityUI(null, '', 'Escolha um Vídeo Portador (arquivo ou link) acima para calcular a capacidade em GBs.');
        return;
      }
    } else if (durSrc === 'audio') {
      const aMode = $('up-audio-mode').value;
      if (aMode === 'file') {
        src = $('up-audio-source').value.trim();
        if (!src) {
          renderCapacityUI(null, '', 'Escolha um arquivo de áudio ou cole um link de música acima para calcular a capacidade em GBs.');
          return;
        }
      } else if (aMode === 'host') {
        src = $('up-host-video').value.trim();
        if (!src) {
          renderCapacityUI(null, '', 'Informe o Vídeo Portador acima (ou mude Trilha de Áudio para "Arquivo/Link") para calcular pela duração do áudio.');
          return;
        }
      } else {
        renderCapacityUI(null, '', 'Mude "Trilha de Áudio" acima para "Arquivo de Áudio Local ou Link/URL" (ou "Áudio do Vídeo Portador") para usar a música como duração mandatória.');
        return;
      }
    }

    if (!forceProbe && state.mediaDurCache[src]) {
      renderCapacityUI(state.mediaDurCache[src]);
      return;
    }
    renderCapacityUI(null, 'Calculando duração e capacidade...');
    try {
      const info = await api('/api/media/probe-duration', { method: 'POST', body: JSON.stringify({ source: src }) });
      state.mediaDurCache[src] = info;
      renderCapacityUI(info);
    } catch (err) {
      renderCapacityUI(null, '', '⚠️ ' + err.message);
    }
  };

  const scheduleCapacityProbe = () => {
    clearTimeout(mediaProbeTimer);
    if ($('up-duration-source').value === 'files') {
      renderCapacityUI(null);
      return;
    }
    mediaProbeTimer = setTimeout(() => refreshCapacityMeter(false), 650);
  };

  const getValidFPSOptions = (res, prof) => {
    // Empirically validated FPS limits per (resolution, profile) so users never pick a broken combo!
    if (prof === 'y8-gray') {
      if (res === '4k') return [{ v: 8, l: '8 FPS (Extra-Seguro)' }, { v: 10, l: '10 FPS (⭐ Recomendado)', rec: true }];
      if (res === '2k') return [{ v: 10, l: '10 FPS (Extra-Seguro)' }, { v: 15, l: '15 FPS (⭐ Recomendado — 3,5× AV1)', rec: true }];
      if (res === '1080p') return [{ v: 10, l: '10 FPS (⭐ Recomendado — 0 B erro)', rec: true }, { v: 15, l: '15 FPS (Mais compacto)' }];
      if (res === '720p') return [{ v: 10, l: '10 FPS (Extra-Seguro)' }, { v: 15, l: '15 FPS (⭐ Recomendado)', rec: true }];
    }
    if (prof === 'y6-gray') {
      if (res === '4k') return [{ v: 8, l: '8 FPS (Extra-Seguro)' }, { v: 10, l: '10 FPS (⭐ Recomendado)', rec: true }];
      if (res === '2k') return [{ v: 10, l: '10 FPS (Extra-Seguro — 0 B erro)' }, { v: 15, l: '15 FPS (⭐ Recomendado — 3,5× AV1)', rec: true }, { v: 20, l: '20 FPS (Compacto)' }];
      if (res === '1080p') return [{ v: 10, l: '10 FPS (Extra-Seguro)' }, { v: 15, l: '15 FPS (Balanceado)' }, { v: 20, l: '20 FPS (⭐ Compacto)', rec: true }];
      if (res === '720p') return [{ v: 10, l: '10 FPS (Extra-Seguro)' }, { v: 15, l: '15 FPS (Balanceado)' }, { v: 20, l: '20 FPS (⭐ Recomendado)', rec: true }];
    }
    if (prof === 'y4-gray') {
      if (res === '4k') return [{ v: 8, l: '8 FPS' }, { v: 10, l: '10 FPS (⭐ Recomendado)', rec: true }, { v: 15, l: '15 FPS (Compacto)' }];
      if (res === '2k') return [{ v: 10, l: '10 FPS' }, { v: 15, l: '15 FPS (Balanceado)' }, { v: 24, l: '24 FPS (⭐ Compacto)', rec: true }];
      if (res === '1080p') return [{ v: 10, l: '10 FPS' }, { v: 15, l: '15 FPS' }, { v: 24, l: '24 FPS (⭐ Recomendado)', rec: true }, { v: 30, l: '30 FPS (Compacto)' }];
      if (res === '720p') return [
        { v: 15, l: '15 FPS (Extra-Seguro)' },
        { v: 24, l: '24 FPS (Seguro)' },
        { v: 30, l: '30 FPS (⭐ Recorde 3,5× AV1 · 4,4× H.264)', rec: true },
        { v: 60, l: '60 FPS (⚠️ Experimental — só VP9)' }
      ];
    }
    if (prof === 'y2-safe') {
      if (res === '720p' || res === '1080p') return [{ v: 15, l: '15 FPS' }, { v: 24, l: '24 FPS' }, { v: 30, l: '30 FPS (⭐ Recomendado)', rec: true }, { v: 60, l: '60 FPS (0 B erro!)' }];
      return [{ v: 10, l: '10 FPS' }, { v: 15, l: '15 FPS (⭐ Recomendado)', rec: true }, { v: 24, l: '24 FPS' }, { v: 30, l: '30 FPS' }];
    }
    if (prof === 'video-fractal' || prof === 'video-chroma' || prof === 'video-bw' || prof.startsWith('stealth-')) {
      return [
        { v: 10, l: '10 FPS (Extra-Seguro)' },
        { v: 15, l: '15 FPS (⭐ Recomendado — 0 B erro)', rec: true },
        { v: 24, l: '24 FPS (Cinema — mais fluido)' },
        { v: 30, l: '30 FPS (Fluidez máxima / 2× capacidade)' }
      ];
    }
    if (prof === 'art-fullres' || prof === 'art-stego' || prof === 'art-chroma' || prof === 'art-crystal' || prof === 'art-fractal3d' ||
        prof === 'art-angles' || prof === 'art-fractal-angles' || prof === 'art-bauhaus' || prof === 'art-constel' || prof === 'art-truchet') {
      if (res === '4k') return [{ v: 8, l: '8 FPS (Extra-Seguro)' }, { v: 10, l: '10 FPS (⭐ Recomendado p/ Arte 4K)', rec: true }, { v: 15, l: '15 FPS (Mais fluido)' }];
      return [{ v: 10, l: '10 FPS (Extra-Seguro)' }, { v: 15, l: '15 FPS (⭐ Recomendado — Validado VP9)', rec: true }, { v: 20, l: '20 FPS (Fluido)' }];
    }
    return [{ v: 8, l: '8 FPS (Mais resistente)' }, { v: 10, l: '10 FPS (⭐ Recomendado)', rec: true }, { v: 15, l: '15 FPS' }];
  };

  const refreshDynamicFPS = (preferredFPS = null) => {
    const res = $('up-resolution').value;
    const prof = $('up-profile').value;
    const opts = getValidFPSOptions(res, prof);
    const prev = preferredFPS != null ? Number(preferredFPS) : Number($('up-fps').value);
    $('up-fps').innerHTML = opts.map(o => `<option value="${o.v}">${esc(o.l)}</option>`).join('');
    const exact = opts.find(o => o.v === prev);
    const rec = opts.find(o => o.rec) || opts[0];
    $('up-fps').value = String(exact ? exact.v : rec.v);
    renderAttrBars('up-attr-bars', res, prof, Number($('up-fps').value), $('up-vcodec').value, 'balanced', false, $('up-cross-fec')?.value || 'auto');
  };

  const refreshCarrierUI = (preferredFPS = null) => {
    const res = $('up-carrier-res').value || '2k';
    const prof = $('up-carrier-profile').value || 'art-fullres';
    const hasHost = ($('up-host-video')?.value.trim() || '') !== '';
    const theme = $('up-art-theme')?.value || 'deepdream-l2';

    // Show/hide optional keyword input when Efeito Artístico = Nenhum
    if ($('up-art-keyword-wrap')) {
      $('up-art-keyword-wrap').style.opacity = theme === 'none' ? '0.45' : '1';
      $('up-art-keyword').disabled = theme === 'none';
    }
    // Enable host audio / video duration options automatically whenever a Vídeo Base is supplied
    if ($('opt-audio-host')) {
      $('opt-audio-host').disabled = !hasHost;
      if (!hasHost && $('up-audio-mode').value === 'host') $('up-audio-mode').value = 'ambient';
    }
    if ($('opt-dur-video')) {
      $('opt-dur-video').disabled = !hasHost;
      if (!hasHost && $('up-duration-source').value === 'video') $('up-duration-source').value = 'files';
    }

    const aMode = $('up-audio-mode').value;
    $('up-audio-file-wrap').classList.toggle('hidden', aMode !== 'file');
    if ($('up-music-studio')) $('up-music-studio').classList.toggle('hidden', aMode !== 'ambient');
    const opts = getValidFPSOptions(res, prof);
    const prev = preferredFPS != null ? Number(preferredFPS) : Number($('up-carrier-fps').value);
    $('up-carrier-fps').innerHTML = opts.map(o => `<option value="${o.v}">${esc(o.l)}</option>`).join('');
    const exact = opts.find(o => o.v === prev);
    const rec = opts.find(o => o.rec) || opts[0];
    $('up-carrier-fps').value = String(exact ? exact.v : rec.v);
    // Hide header bar checkbox applies to all artistic/carrier modes except art-chroma and pure non-host video-bw
    if ($('up-hide-header-row')) {
      const canHide = prof !== 'art-chroma' && (prof !== 'video-bw' || hasHost);
      $('up-hide-header').disabled = !canHide;
      $('up-hide-header-row').style.opacity = canHide ? '1' : '0.45';
    }
    updateProfileDesc('up-carrier-profile', 'up-carrier-prof-desc');
    renderAttrBars('up-carrier-attr-bars', res, prof, Number($('up-carrier-fps').value), $('up-carrier-vcodec').value, $('up-mask-intensity')?.value || 'balanced', !!$('up-hide-header')?.checked, $('up-cross-fec')?.value || 'auto', $('up-host-video')?.value || '');
    updateStylePreviewGIF();
    refreshCapacityMeter(false);
  };

  const highlightActivePreset = () => {
    const res = $('up-resolution').value, prof = $('up-profile').value, fps = Number($('up-fps').value);
    const map = {
      balanced: res === '2k' && prof === 'y8-gray' && fps === 15,
      smallest: res === '720p' && prof === 'y4-gray' && fps === 30,
      safe: res === '2k' && prof === 'y6-gray' && fps === 10
    };
    document.querySelectorAll('[data-up-preset]').forEach(b => {
      b.classList.toggle('primary', !!map[b.dataset.upPreset]);
    });
  };

  const validResCodes = new Set(['2k', '4k', '1080p', '720p']);
  const normalizeCodecSelect = (c) => {
    if (c === 'av1') return 'av1-mp4';
    if (c === 'h264') return 'h264-mp4';
    return c || 'auto';
  };
  $('btn-open-upload').onclick = () => {
    const defRes = state.vault.settings.default_resolution;
    const defProf = state.vault.settings.default_profile || 'y8-gray';
    const defCodec = normalizeCodecSelect(state.vault.settings.default_video_codec);
    const isArtProf = defProf.startsWith('art-');
    const isVideoProf = defProf.startsWith('video-');
    const isStealthProf = defProf.startsWith('stealth-');
    const isCarrierProf = isArtProf || isVideoProf || isStealthProf;

    $('up-resolution').value = validResCodes.has(defRes) ? defRes : '2k';
    $('up-profile').value = isCarrierProf ? 'y8-gray' : defProf;
    $('up-vcodec').value = defCodec;
    updateProfileDesc('up-profile', 'up-prof-desc');
    refreshDynamicFPS();
    highlightActivePreset();

    $('up-carrier-res').value = validResCodes.has(defRes) ? defRes : '2k';
    $('up-carrier-profile').value = isCarrierProf ? defProf : 'art-fullres';
    $('up-carrier-vcodec').value = 'auto';
    if ($('up-art-theme').querySelector('option[value="deepdream-l2"]')) {
      $('up-art-theme').value = 'deepdream-l2';
    } else if ($('up-art-theme').querySelector('option[value="fractal"]')) {
      $('up-art-theme').value = 'fractal';
    }

    setUploadTab(isCarrierProf ? 'artistic' : 'create');
    $('up-dest-folder').value = state.filters.folder_id || 'root';
    $('up-transfer-box').classList.add('hidden');
    $('btn-start-upload').disabled = false;
    openModal('modal-upload');
  };
  document.querySelectorAll('[data-up-tab]').forEach(b => b.onclick = () => setUploadTab(b.dataset.upTab));
  $('up-resolution').onchange = () => { refreshDynamicFPS(); highlightActivePreset(); };
  $('up-profile').onchange = () => { updateProfileDesc('up-profile', 'up-prof-desc'); refreshDynamicFPS(); highlightActivePreset(); };
  $('up-vcodec').onchange = () => {
    renderAttrBars('up-attr-bars', $('up-resolution').value, $('up-profile').value, Number($('up-fps').value), $('up-vcodec').value, 'balanced', false, $('up-cross-fec')?.value || 'auto');
  };
  const onUpFPSChange = () => {
    renderAttrBars('up-attr-bars', $('up-resolution').value, $('up-profile').value, Number($('up-fps').value), $('up-vcodec').value, 'balanced', false, $('up-cross-fec')?.value || 'auto');
    highlightActivePreset();
  };
  $('up-fps').onchange = onUpFPSChange;
  $('up-fps').oninput = onUpFPSChange;
  $('up-carrier-profile').onchange = () => {
    const p = $('up-carrier-profile').value;
    const hasHost = ($('up-host-video')?.value.trim() || '') !== '';
    if ((p === 'video-fractal' || p === 'video-chroma' || (p.startsWith('stealth-') && hasHost)) && $('up-audio-mode').value === 'ambient') {
      $('up-audio-mode').value = 'host';
    } else if ((p === 'art-stego' || p === 'art-chroma') && $('up-audio-mode').value === 'host') {
      $('up-audio-mode').value = 'ambient';
    }
    if (p.startsWith('stealth-') && $('up-art-theme')?.querySelector('option[value="none"]')) {
      // Default to pure mask ("Nenhum" art theme) so the stealth preview immediately shows clean overlay!
      if ($('up-art-theme').value !== 'none' && !hasHost) {
        $('up-art-theme').value = 'none';
      }
    }
    refreshCarrierUI();
  };
  $('up-carrier-res').onchange = () => refreshCarrierUI();
  $('up-carrier-vcodec').onchange = () => refreshCarrierUI();
  const onCarrierFPSChange = () => {
    renderAttrBars('up-carrier-attr-bars', $('up-carrier-res').value, $('up-carrier-profile').value, Number($('up-carrier-fps').value), $('up-carrier-vcodec').value, $('up-mask-intensity')?.value || 'balanced', !!$('up-hide-header')?.checked, $('up-cross-fec')?.value || 'auto', $('up-host-video')?.value || '');
    refreshCapacityMeter(false);
  };
  $('up-carrier-fps').onchange = onCarrierFPSChange;
  $('up-carrier-fps').oninput = onCarrierFPSChange;
  if ($('up-mask-intensity')) $('up-mask-intensity').onchange = () => { onCarrierFPSChange(); updateStylePreviewGIF(); };
  if ($('up-hide-header')) $('up-hide-header').onchange = () => { onCarrierFPSChange(); updateStylePreviewGIF(); };
  if ($('up-cross-fec')) {
    $('up-cross-fec').onchange = () => {
      onUpFPSChange();
      onCarrierFPSChange();
    };
  }
  $('up-art-theme').onchange = () => {
    const theme = $('up-art-theme').value;
    const hasHost = ($('up-host-video')?.value.trim() || '') !== '';
    const p = $('up-carrier-profile').value;
    if (theme === 'none' && !hasHost && (p === 'art-fullres' || p === 'art-fractal3d' || p === 'video-fractal')) {
      $('up-carrier-profile').value = 'art-crystal';
      toast('Sem efeito artístico e sem vídeo base: alternado para Cristal 3D geométrico.');
    }
    refreshCarrierUI();
  };
  $('up-audio-mode').onchange = () => refreshCarrierUI();
  $('up-duration-source').onchange = () => {
    const ds = $('up-duration-source').value;
    if (ds === 'audio' && ($('up-audio-mode').value === 'none' || $('up-audio-mode').value === 'ambient')) {
      $('up-audio-mode').value = 'file';
    }
    refreshCarrierUI();
  };
  $('btn-reprobe-cap').onclick = () => refreshCapacityMeter(true);
  const onHostVideoChanged = (autoSwitch = true) => {
    const p = $('up-carrier-profile').value;
    const hasHost = $('up-host-video').value.trim() !== '';
    const theme = $('up-art-theme')?.value || 'deepdream-l2';
    if (autoSwitch && hasHost && (p === 'art-fullres' || p === 'art-fractal3d')) {
      $('up-carrier-profile').value = theme === 'none' ? 'art-fractal-angles' : 'video-fractal';
      toast('Vídeo Base detectado: padrão ajustado para manter seu vídeo 100% visível em decodificação cega!');
    }
    if (hasHost && $('up-audio-mode').value === 'ambient') {
      $('up-audio-mode').value = 'host';
    }
    refreshCarrierUI();
    scheduleCapacityProbe();
  };
  $('up-host-video').oninput = () => onHostVideoChanged(true);
  $('up-audio-source').oninput = () => scheduleCapacityProbe();

  $('btn-pick-host-video').onclick = async () => {
    try {
      const res = await api('/api/system/pick-media?kind=video', { method: 'POST' });
      if (!res.cancelled && res.path) {
        $('up-host-video').value = res.path;
        onHostVideoChanged(true);
        refreshCapacityMeter(true);
      }
    } catch (err) { toast('❌ ' + err.message, 5000); }
  };
  $('btn-clear-host-video').onclick = () => { $('up-host-video').value = ''; onHostVideoChanged(false); };
  $('btn-pick-audio-file').onclick = async () => {
    try {
      const res = await api('/api/system/pick-media?kind=audio', { method: 'POST' });
      if (!res.cancelled && res.path) {
        $('up-audio-source').value = res.path;
        refreshCapacityMeter(true);
      }
    } catch (err) { toast('❌ ' + err.message, 5000); }
  };

  // Generative Music Studio controls & live audio preview
  state.musicSeed = Math.floor(Math.random() * 900000000) + 100000;
  const musicStyleDefaults = {
    lofi: ['rhodes', 'bass', 'drums', 'vinyl'],
    ambient: ['pad', 'strings', 'bells', 'rain'],
    synthwave: ['pad', 'guitar', 'bass', 'drums'],
    meditation: ['pad', 'bowls', 'rain'],
    nature: ['guitar', 'harp', 'ocean'],
    piano: ['piano', 'strings', 'bass'],
    chiptune: ['harp', 'bass', 'drums', 'bells']
  };
  const getSelectedMusicInstruments = () => {
    const box = $('up-music-insts');
    if (!box) return [];
    return [...box.querySelectorAll('input[type="checkbox"]:checked')].map(c => c.value);
  };
  const applyMusicStyleDefaults = (style) => {
    const box = $('up-music-insts');
    if (!box) return;
    const def = new Set(musicStyleDefaults[style] || musicStyleDefaults.ambient);
    box.querySelectorAll('input[type="checkbox"]').forEach(c => { c.checked = def.has(c.value); });
  };
  const playMusicPreview = () => {
    const player = $('up-music-player');
    if (!player) return;
    const style = $('up-music-style')?.value || 'ambient';
    const mood = $('up-music-mood')?.value || 'dreamy';
    const prompt = $('up-music-prompt')?.value.trim() || '';
    const insts = getSelectedMusicInstruments();
    if (!insts.length) return toast('Selecione pelo menos 1 instrumento para ouvir a prévia.');
    const url = `/api/preview/audio?style=${encodeURIComponent(style)}&mood=${encodeURIComponent(mood)}&prompt=${encodeURIComponent(prompt)}&instruments=${encodeURIComponent(insts.join(','))}&seed=${state.musicSeed}`;
    player.classList.remove('hidden');
    player.src = url;
    player.play().catch(() => { });
  };
  if ($('up-music-style')) {
    $('up-music-style').onchange = () => {
      applyMusicStyleDefaults($('up-music-style').value);
      if ($('up-music-player') && !$('up-music-player').classList.contains('hidden')) playMusicPreview();
    };
  }
  if ($('up-music-mood')) {
    $('up-music-mood').onchange = () => {
      if ($('up-music-player') && !$('up-music-player').classList.contains('hidden')) playMusicPreview();
    };
  }
  if ($('btn-preview-music')) $('btn-preview-music').onclick = () => playMusicPreview();
  if ($('btn-reroll-music')) {
    $('btn-reroll-music').onclick = () => {
      state.musicSeed = Math.floor(Math.random() * 900000000) + 100000;
      playMusicPreview();
    };
  }
  if ($('up-music-prompt')) {
    $('up-music-prompt').onkeydown = (e) => {
      if (e.key === 'Enter') { e.preventDefault(); playMusicPreview(); }
    };
  }

  document.querySelectorAll('[data-up-preset]').forEach(b => b.onclick = () => {
    const presets = {
      balanced: { res: '2k', prof: 'y8-gray', fps: 15, vcodec: 'auto' },
      smallest: { res: '720p', prof: 'y4-gray', fps: 30, vcodec: 'auto' },
      safe: { res: '2k', prof: 'y6-gray', fps: 10, vcodec: 'auto' }
    };
    const p = presets[b.dataset.upPreset];
    if (!p) return;
    $('up-resolution').value = p.res;
    $('up-profile').value = p.prof;
    $('up-vcodec').value = p.vcodec;
    updateProfileDesc('up-profile', 'up-prof-desc');
    refreshDynamicFPS(p.fps);
    highlightActivePreset();
  });
  const updateSettingsAttrs = () => {
    const res = $('set-res').value, prof = $('set-prof').value;
    const opts = getValidFPSOptions(res, prof);
    const rec = opts.find(o => o.rec) || opts[0];
    renderAttrBars('set-attr-bars', res, prof, rec.v, $('set-vcodec').value);
  };
  $('set-res').onchange = () => updateSettingsAttrs();
  $('set-prof').onchange = () => { updateProfileDesc('set-prof', 'set-prof-desc'); updateSettingsAttrs(); };
  $('set-vcodec').onchange = () => updateSettingsAttrs();

  // Unified File Pickers (Windows native + browser picker + typed path all in one!)
  const showSourceInfo = (html) => {
    const info = $('up-source-info');
    if (!html) { info.classList.add('hidden'); return; }
    info.classList.remove('hidden');
    info.innerHTML = html;
  };
  $('btn-native-pick-folder').onclick = async () => {
    showSourceInfo('⏳ Aguardando seleção de pasta no Windows...');
    try {
      const res = await api('/api/system/pick-folder', { method: 'POST' });
      if (res.cancelled) { showSourceInfo(''); return; }
      state.uploadMode = 'native';
      state.pendingNativePaths = [res.path];
      state.pendingUploadFiles = [];
      state.selectedPayloadBytes = res.total_bytes || 0;
      $('up-local-path').value = res.path;
      showSourceInfo(`✅ <b>Pasta pronta:</b> <code>${esc(res.path)}</code> · ${res.file_count} arquivo(s) · <b>${fmtBytes(res.total_bytes)}</b> (leitura direta do disco!)`);
      refreshCapacityMeter(false);
    } catch (err) {
      showSourceInfo('❌ ' + esc(err.message));
    }
  };
  $('btn-native-pick-files').onclick = async () => {
    showSourceInfo('⏳ Aguardando seleção de arquivos no Windows...');
    try {
      const res = await api('/api/system/pick-files', { method: 'POST' });
      if (res.cancelled) { showSourceInfo(''); return; }
      state.uploadMode = 'native';
      state.pendingNativePaths = res.paths;
      state.pendingUploadFiles = [];
      state.selectedPayloadBytes = res.total_bytes || 0;
      $('up-local-path').value = res.paths.length === 1 ? res.paths[0] : `${res.file_count} arquivo(s) selecionado(s)`;
      showSourceInfo(`✅ <b>${res.file_count} arquivo(s) pronto(s)</b> · <b>${fmtBytes(res.total_bytes)}</b> (leitura direta do disco!)`);
      refreshCapacityMeter(false);
    } catch (err) {
      showSourceInfo('❌ ' + esc(err.message));
    }
  };
  $('up-local-path').oninput = () => {
    const v = $('up-local-path').value.trim();
    clearTimeout(pathStatsTimer);
    if (v) {
      state.uploadMode = 'localpath';
      state.pendingNativePaths = [];
      state.pendingUploadFiles = [];
      showSourceInfo(`📂 <b>Caminho informado:</b> <code>${esc(v)}</code>`);
      pathStatsTimer = setTimeout(async () => {
        try {
          const st = await api('/api/system/path-stats', { method: 'POST', body: JSON.stringify({ path: v }) });
          state.selectedPayloadBytes = st.total_bytes || 0;
          showSourceInfo(`✅ <b>Caminho pronto:</b> ${st.file_count} arquivo(s) · <b>${fmtBytes(st.total_bytes)}</b>`);
          refreshCapacityMeter(false);
        } catch {
          state.selectedPayloadBytes = 0;
          refreshCapacityMeter(false);
        }
      }, 500);
    } else {
      state.selectedPayloadBytes = 0;
      showSourceInfo('');
      refreshCapacityMeter(false);
    }
  };

  const onPick = (files) => {
    state.uploadMode = 'picker';
    state.pendingNativePaths = [];
    state.pendingUploadFiles = [...files];
    const tot = state.pendingUploadFiles.reduce((a, f) => a + f.size, 0);
    state.selectedPayloadBytes = tot;
    $('up-local-path').value = '';
    showSourceInfo(`✅ <b>${state.pendingUploadFiles.length} arquivo(s)</b> selecionado(s) pelo navegador (<b>${fmtBytes(tot)}</b>)`);
    refreshCapacityMeter(false);
  };
  $('up-files-input').onchange = (e) => onPick(e.target.files);
  $('up-folder-input').onchange = (e) => onPick(e.target.files);

  function uploadFormDataWithProgress(fd) {
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      const t0 = Date.now();
      const box = $('up-transfer-box');
      box.classList.remove('hidden');
      $('up-transfer-label').textContent = 'Carregando arquivos para o motor local...';
      $('up-transfer-pct').textContent = '0%';
      $('up-transfer-bar').style.width = '0%';
      $('up-transfer-eta').textContent = 'Calculando velocidade e tempo restante...';

      xhr.upload.onprogress = (ev) => {
        if (!ev.lengthComputable) return;
        const pct = Math.min(100, Math.round((ev.loaded / ev.total) * 100));
        const elapsed = (Date.now() - t0) / 1000;
        const bps = elapsed > 0.1 ? ev.loaded / elapsed : 0;
        const remSecs = bps > 0 ? (ev.total - ev.loaded) / bps : 0;
        $('up-transfer-pct').textContent = `${pct}%`;
        $('up-transfer-bar').style.width = `${pct}%`;
        $('up-transfer-eta').textContent = `${fmtBytes(ev.loaded)} / ${fmtBytes(ev.total)} · ${fmtBytes(bps)}/s · ⏱ ${fmtETA(remSecs)}`;
      };
      xhr.onload = () => {
        let data = {};
        try { data = JSON.parse(xhr.responseText); } catch { }
        if (xhr.status >= 200 && xhr.status < 300) resolve(data);
        else reject(new Error(data.error || `Erro HTTP ${xhr.status}`));
      };
      xhr.onerror = () => reject(new Error('Falha na transferência — use o Seletor do Windows para pastas grandes!'));
      xhr.open('POST', '/api/upload/files');
      xhr.withCredentials = true;
      xhr.send(fd);
    });
  }

  $('btn-start-upload').onclick = async () => {
    const btn = $('btn-start-upload');
    btn.disabled = true;
    try {
      const isCarrier = state.uploadTab === 'artistic';
      const isPublicMode = !!$('up-public-mode')?.checked;
      let publicPasswords = [];
      if (isPublicMode) {
        publicPasswords = [...document.querySelectorAll('#up-public-pw-list .pub-pw-input')].map(i => i.value).filter(v => v.length > 0);
        if (!publicPasswords.length) {
          btn.disabled = false;
          return toast('Digite pelo menos 1 senha para gerar o vídeo em Modo Público.');
        }
      }
      const commonOpts = {
        folder_id: $('up-dest-folder').value,
        resolution: isCarrier ? $('up-carrier-res').value : $('up-resolution').value,
        profile: isCarrier ? $('up-carrier-profile').value : $('up-profile').value,
        video_codec: isCarrier ? $('up-carrier-vcodec').value : $('up-vcodec').value,
        fps: parseInt(isCarrier ? $('up-carrier-fps').value : $('up-fps').value, 10),
        auto_upload_yt: $('up-auto-yt').checked,
        privacy: $('up-privacy')?.value || '',
        video_title: $('up-video-title')?.value.trim() || '',
        art_theme: isCarrier ? $('up-art-theme').value : '',
        art_keyword: isCarrier ? $('up-art-keyword').value.trim() : '',
        mask_intensity: isCarrier ? ($('up-mask-intensity')?.value || 'balanced') : 'balanced',
        hide_header_bar: isCarrier ? !!$('up-hide-header')?.checked : false,
        host_video_source: isCarrier ? $('up-host-video').value.trim() : '',
        audio_mode: isCarrier ? $('up-audio-mode').value : 'none',
        audio_source: isCarrier ? $('up-audio-source').value.trim() : '',
        music_style: isCarrier ? ($('up-music-style')?.value || 'ambient') : '',
        music_mood: isCarrier ? ($('up-music-mood')?.value || 'dreamy') : '',
        music_instruments: isCarrier ? getSelectedMusicInstruments() : [],
        music_prompt: isCarrier ? ($('up-music-prompt')?.value.trim() || '') : '',
        music_seed: isCarrier ? (state.musicSeed || 0) : 0,
        duration_source: isCarrier ? $('up-duration-source').value : 'files',
        padding_mode: isCarrier ? ($('up-padding-mode')?.value || 'auto') : 'start',
        cross_fec_mode: $('up-cross-fec')?.value || 'auto',
        public_passwords: publicPasswords
      };

      if (state.pendingNativePaths.length) {
        await api('/api/upload/paths', {
          method: 'POST',
          body: JSON.stringify({ paths: state.pendingNativePaths, ...commonOpts })
        });
        state.pendingNativePaths = [];
        $('up-local-path').value = '';
        showSourceInfo('');
      } else if (state.pendingUploadFiles.length) {
        const fd = new FormData();
        Object.entries(commonOpts).forEach(([k, v]) => {
          let val;
          if (k === 'public_passwords') val = JSON.stringify(v);
          else if (Array.isArray(v)) val = v.join(',');
          else if (typeof v === 'boolean') val = v ? 'true' : 'false';
          else val = String(v);
          fd.append(k, val);
        });
        for (const f of state.pendingUploadFiles) {
          fd.append('rel_paths', f.webkitRelativePath || f.name);
          fd.append('mod_times', new Date(f.lastModified).toISOString().slice(0, 19).replace('T', ' '));
          fd.append('files', f, f.name);
        }
        await uploadFormDataWithProgress(fd);
        state.pendingUploadFiles = [];
        showSourceInfo('');
      } else if ($('up-local-path').value.trim()) {
        await api('/api/upload/path', {
          method: 'POST',
          body: JSON.stringify({ path: $('up-local-path').value.trim(), ...commonOpts })
        });
        $('up-local-path').value = '';
        showSourceInfo('');
      } else {
        btn.disabled = false;
        return toast('Escolha uma pasta/arquivos ou cole um caminho primeiro.');
      }
      if ($('up-video-title')) $('up-video-title').value = '';
      closeModals();
      $('jobs-drawer').classList.remove('hidden');
      pollJobs();
    } catch (err) {
      toast('❌ ' + err.message, 6000);
    } finally {
      btn.disabled = false;
      $('up-transfer-box').classList.add('hidden');
    }
  };

  // Drag & drop directly onto main pane
  const dz = $('drop-zone');
  dz.ondragover = (e) => { e.preventDefault(); dz.classList.add('drag-over'); };
  dz.ondragleave = () => dz.classList.remove('drag-over');
  dz.ondrop = (e) => {
    e.preventDefault(); dz.classList.remove('drag-over');
    if (e.dataTransfer.files?.length) {
      onPick(e.dataTransfer.files);
      $('btn-open-upload').click();
    }
  };

  // Upload modal: Public Share layered password controls (1..5 passwords)
  const addLayeredPasswordRow = (containerId, inputCls) => {
    const box = $(containerId);
    if (!box) return;
    const count = box.querySelectorAll('.' + inputCls).length;
    if (count >= 5) return toast('Máximo de 5 senhas em cascata Pós-IA.');
    const curType = box.querySelector('.' + inputCls)?.type || 'password';
    const row = document.createElement('div');
    row.className = 'row-inline';
    row.innerHTML = `<input type="${curType}" class="${inputCls}" placeholder="Senha ${count + 1} (Camada Pós-IA extra)" style="flex:1"><button type="button" class="btn sm danger-ghost" title="Remover senha">✕</button>`;
    row.querySelector('button').onclick = () => row.remove();
    box.appendChild(row);
    row.querySelector('input').focus();
  };
  const togglePasswordVisibility = (containerId, inputCls) => {
    const inputs = [...document.querySelectorAll(`#${containerId} .${inputCls}`)];
    if (!inputs.length) return;
    const nextType = inputs[0].type === 'password' ? 'text' : 'password';
    inputs.forEach(i => { i.type = nextType; });
  };

  if ($('up-public-mode')) {
    $('up-public-mode').onchange = () => {
      $('up-public-pw-wrap').classList.toggle('hidden', !$('up-public-mode').checked);
    };
  }
  if ($('btn-up-add-pub-pw')) $('btn-up-add-pub-pw').onclick = () => addLayeredPasswordRow('up-public-pw-list', 'pub-pw-input');
  if ($('btn-up-toggle-pub-pw')) $('btn-up-toggle-pub-pw').onclick = () => togglePasswordVisibility('up-public-pw-list', 'pub-pw-input');

  // Public Download Modal (works BOTH before login/without any vault AND inside the app!)
  let pubJobPollTimer = null;
  let pubLastManifest = null;
  const openPublicDownloadModal = () => {
    $('pub-error').classList.add('hidden');
    $('pub-progress-box').classList.add('hidden');
    openModal('modal-public-download');
  };
  if ($('btn-auth-public-dl')) $('btn-auth-public-dl').onclick = openPublicDownloadModal;
  if ($('btn-open-public-dl')) $('btn-open-public-dl').onclick = openPublicDownloadModal;
  if ($('btn-pub-add-pw')) $('btn-pub-add-pw').onclick = () => addLayeredPasswordRow('pub-dl-pw-list', 'pub-dl-pw-input');
  if ($('btn-pub-toggle-pw')) $('btn-pub-toggle-pw').onclick = () => togglePasswordVisibility('pub-dl-pw-list', 'pub-dl-pw-input');

  if ($('btn-pub-pick-video')) {
    $('btn-pub-pick-video').onclick = async () => {
      try {
        const res = await api('/api/system/pick-media?kind=video', { method: 'POST' });
        if (!res.cancelled && res.path) $('pub-dl-source').value = res.path;
      } catch (err) { toast('❌ ' + err.message, 4000); }
    };
  }
  if ($('btn-pub-pick-dest')) {
    $('btn-pub-pick-dest').onclick = async () => {
      try {
        const res = await api('/api/system/pick-folder', { method: 'POST' });
        if (!res.cancelled && res.path) $('pub-dl-dest').value = res.path;
      } catch (err) { toast('❌ ' + err.message, 4000); }
    };
  }

  const getPublicDownloadPasswords = () => [...document.querySelectorAll('#pub-dl-pw-list .pub-dl-pw-input')].map(i => i.value).filter(v => v.length > 0);
  const pollPublicJob = (jobId, onDone) => {
    clearInterval(pubJobPollTimer);
    $('pub-error').classList.add('hidden');
    $('pub-progress-box').classList.remove('hidden');
    $('pub-prog-title').textContent = 'Iniciando...';
    $('pub-prog-pct').textContent = '0%';
    $('pub-prog-bar').style.width = '0%';
    $('pub-prog-detail').textContent = '';

    const tick = async () => {
      try {
        const { jobs } = await api('/api/jobs');
        const j = (jobs || []).find(x => x.id === jobId);
        if (!j) return;
        $('pub-prog-title').textContent = j.title || 'Processando...';
        $('pub-prog-pct').textContent = `${j.progress || 0}%`;
        $('pub-prog-bar').style.width = `${j.progress || 0}%`;
        const extra = [j.speed_text || j.speed, j.eta_text || j.eta].filter(Boolean).join(' · ');
        $('pub-prog-detail').textContent = (j.detail || j.message || '') + (extra ? ` (${extra})` : '');
        if (j.status === 'done') {
          clearInterval(pubJobPollTimer);
          onDone(j.result || {});
        } else if (j.status === 'error') {
          clearInterval(pubJobPollTimer);
          $('pub-error').textContent = '❌ ' + (j.error || 'Erro desconhecido');
          $('pub-error').classList.remove('hidden');
        }
      } catch { }
    };
    pubJobPollTimer = setInterval(tick, 900);
    tick();
  };

  const startPublicExtract = async (relPaths = []) => {
    const src = $('pub-dl-source').value.trim();
    const pws = getPublicDownloadPasswords();
    if (!src) return toast('Informe o link do YouTube ou caminho do vídeo local.');
    if (!pws.length) return toast('Digite a(s) senha(s) do vídeo público.');
    $('pub-error').classList.add('hidden');
    try {
      const job = await api('/api/public/extract', {
        method: 'POST',
        body: JSON.stringify({ source: src, passwords: pws, rel_paths: relPaths, dest_dir: $('pub-dl-dest').value.trim() })
      });
      pollPublicJob(job.id, (res) => {
        const box = $('pub-result');
        box.classList.remove('hidden');
        box.innerHTML = `<div class="hint-box" style="border-color:#22c55e"><b>✅ ${res.count || 0} arquivo(s) extraído(s) com sucesso!</b><br><span class="muted sm-txt">Salvos em: <code>${esc(res.dest_dir || '')}</code> (${res.password_layers || pws.length} camada(s) Pós-Quântica verificada(s))</span></div>`;
        toast(`✅ ${res.count || 0} arquivo(s) baixado(s) do vídeo público!`, 5000);
      });
    } catch (err) {
      $('pub-error').textContent = '❌ ' + err.message;
      $('pub-error').classList.remove('hidden');
    }
  };

  if ($('btn-pub-peek')) {
    $('btn-pub-peek').onclick = async () => {
      const src = $('pub-dl-source').value.trim();
      const pws = getPublicDownloadPasswords();
      if (!src) return toast('Informe o link do YouTube ou caminho do vídeo local.');
      if (!pws.length) return toast('Digite a(s) senha(s) do vídeo público.');
      $('pub-error').classList.add('hidden');
      $('pub-result').classList.add('hidden');
      try {
        const job = await api('/api/public/peek', {
          method: 'POST',
          body: JSON.stringify({ source: src, passwords: pws })
        });
        pollPublicJob(job.id, (res) => {
          pubLastManifest = res.manifest;
          const files = res.manifest?.files || [];
          const box = $('pub-result');
          box.classList.remove('hidden');
          box.innerHTML = `
            <div class="hint-box" style="margin-bottom:8px">
              <b>🌐 Vídeo Público Desbloqueado!</b> (${esc(res.resolution)} · ${esc(res.profile_code)} · <b>${res.password_layers} camada(s) Pós-IA</b> · <b>${files.length} arquivo(s)</b>)
            </div>
            <div style="max-height:220px;overflow:auto;display:flex;flex-direction:column;gap:5px">
              ${files.map(f => `
                <label class="check-row" style="padding:5px 8px;background:rgba(255,255,255,.04);border-radius:6px;margin:0">
                  <input type="checkbox" class="pub-peek-chk" value="${esc(f.rel_path)}" checked>
                  ${f.preview_b64 ? `<img src="data:image/jpeg;base64,${f.preview_b64}" style="width:32px;height:32px;object-fit:cover;border-radius:4px">` : '<span>📄</span>'}
                  <span style="flex:1"><b>${esc(f.rel_path)}</b></span>
                  <span class="muted sm-txt">${fmtBytes(f.size_bytes)}</span>
                </label>`).join('')}
            </div>
            <button type="button" class="btn primary full" id="btn-pub-extract-sel" style="margin-top:8px">⬇️ Baixar Arquivos Selecionados</button>`;
          $('btn-pub-extract-sel').onclick = () => {
            const sel = [...box.querySelectorAll('.pub-peek-chk:checked')].map(c => c.value);
            if (!sel.length) return toast('Selecione pelo menos 1 arquivo.');
            startPublicExtract(sel);
          };
        });
      } catch (err) {
        $('pub-error').textContent = '❌ ' + err.message;
        $('pub-error').classList.remove('hidden');
      }
    };
  }
  if ($('btn-pub-extract')) $('btn-pub-extract').onclick = () => startPublicExtract([]);

  // Public Download Modal: Tab switching (Video + Password vs Folder Share Code YTFOLDER1)
  document.querySelectorAll('[data-pub-tab]').forEach(b => {
    b.onclick = () => {
      const t = b.dataset.pubTab;
      document.querySelectorAll('[data-pub-tab]').forEach(x => x.classList.toggle('active', x === b));
      $('pub-tab-video')?.classList.toggle('hidden', t !== 'video');
      $('pub-tab-folder')?.classList.toggle('hidden', t !== 'folder');
      $('pub-error')?.classList.add('hidden');
      $('pub-result')?.classList.add('hidden');
    };
  });
  if ($('btn-pub-folder-pick-dest')) {
    $('btn-pub-folder-pick-dest').onclick = async () => {
      try {
        const res = await api('/api/system/pick-folder', { method: 'POST' });
        if (!res.cancelled && res.path) $('pub-folder-dest').value = res.path;
      } catch (err) { toast('❌ ' + err.message, 4000); }
    };
  }
  if ($('btn-pub-folder-inspect')) {
    $('btn-pub-folder-inspect').onclick = async () => {
      const code = $('pub-folder-code').value.trim();
      if (!code) return toast('Cole o código da pasta (BVFOLDER1:...).');
      $('pub-error').classList.add('hidden');
      $('pub-result').classList.add('hidden');
      try {
        const res = await api('/api/public/folder-inspect', {
          method: 'POST',
          body: JSON.stringify({ code })
        });
        const box = $('pub-result');
        box.classList.remove('hidden');
        const files = res.files || [];
        box.innerHTML = `
          <div class="hint-box" style="margin-bottom:8px">
            <b>📁 Pasta Compartilhada: ${esc(res.folder_name)}</b><br>
            <span class="muted sm-txt">${res.file_count} arquivo(s) (${fmtBytes(res.total_bytes)}) · ${res.container_count} vídeo(s) no YouTube · Snapshot de ${esc((res.created_at || '').slice(0, 16).replace('T', ' '))}</span>
            ${res.missing_yt_count ? `<div style="color:var(--warn);margin-top:4px">⚠️ ${res.missing_yt_count} vídeo(s) neste código ainda não possuem link do YouTube.</div>` : ''}
          </div>
          <div style="max-height:220px;overflow:auto;display:flex;flex-direction:column;gap:4px">
            ${files.map(f => `
              <div style="display:flex;justify-content:space-between;gap:8px;padding:5px 8px;background:rgba(255,255,255,.04);border-radius:6px;font-size:12px">
                <span>📄 <b>${esc(f.fp ? f.fp + '/' + f.n : f.n)}</b></span>
                <span class="muted">${fmtBytes(f.sz)}</span>
              </div>`).join('')}
          </div>`;
      } catch (err) {
        $('pub-error').textContent = '❌ ' + err.message;
        $('pub-error').classList.remove('hidden');
      }
    };
  }
  if ($('btn-pub-folder-extract')) {
    $('btn-pub-folder-extract').onclick = async () => {
      const code = $('pub-folder-code').value.trim();
      if (!code) return toast('Cole o código da pasta (BVFOLDER1:...).');
      $('pub-error').classList.add('hidden');
      try {
        const job = await api('/api/public/folder-extract', {
          method: 'POST',
          body: JSON.stringify({ code, dest_dir: $('pub-folder-dest').value.trim() })
        });
        pollPublicJob(job.id, (res) => {
          const box = $('pub-result');
          box.classList.remove('hidden');
          box.innerHTML = `<div class="hint-box" style="border-color:#22c55e"><b>✅ Pasta "${esc(res.folder_name || '')}" baixada (${res.count || 0} arquivo(s))!</b><br><span class="muted sm-txt">Salvos em: <code>${esc(res.dest_dir || '')}</code></span></div>`;
          toast(`✅ ${res.count || 0} arquivo(s) extraído(s) da pasta compartilhada!`, 5000);
        });
      } catch (err) {
        $('pub-error').textContent = '❌ ' + err.message;
        $('pub-error').classList.remove('hidden');
      }
    };
  }

  // Link YT modal
  $('btn-link-open-folder').onclick = () => api(`/api/containers/${$('link-cont-id').value}/open-folder`, { method: 'POST' });
  $('btn-save-link-yt').onclick = async () => {
    try {
      const res = await api(`/api/containers/${$('link-cont-id').value}/link`, {
        method: 'POST',
        body: JSON.stringify({
          youtube_url: $('link-yt-url').value,
          verify_online: $('link-verify').checked,
          delete_local: $('link-delete-local').checked
        })
      });
      closeModals();
      toast(`✅ Vídeo vinculado! ${res.detected_height ? `Qualidade detectada: ${res.detected_height}p` : ''}`);
      await refreshVault();
    } catch (err) { toast('❌ ' + err.message, 5000); }
  };

  // Peek modal (async Job with live progress bar + ETA + direct Download/Extract!)
  $('btn-open-peek').onclick = () => {
    $('peek-progress-box').classList.add('hidden');
    $('peek-extract-bar').classList.add('hidden');
    openModal('modal-peek');
  };
  $('btn-run-peek').onclick = async () => {
    $('peek-result').innerHTML = '';
    $('peek-extract-bar').classList.add('hidden');
    state.lastPeekSource = $('peek-source').value.trim();
    try {
      const job = await api('/api/peek', { method: 'POST', body: JSON.stringify({ source: state.lastPeekSource }) });
      state.activePeekJobId = job.id;
      updatePeekProgressUI(job);
      $('jobs-drawer').classList.remove('hidden');
      pollJobs();
    } catch (err) {
      $('peek-progress-box').classList.add('hidden');
      $('peek-result').innerHTML = `<div class="alert error">${esc(err.message)}</div>`;
    }
  };
  $('peek-result').onclick = (e) => {
    const chk = e.target.closest('[data-peek-chk]');
    if (!chk) return;
    const rel = chk.dataset.peekChk;
    if (chk.checked) state.peekSelectedRels.add(rel); else state.peekSelectedRels.delete(rel);
    const n = state.peekSelectedRels.size;
    $('peek-sel-count').textContent = n > 0 ? `${n} arquivo(s) marcado(s)` : `Todos os ${state.lastPeekManifest?.files?.length || 0} arquivo(s)`;
  };
  $('btn-peek-select-all').onclick = () => {
    const boxes = [...$('peek-result').querySelectorAll('[data-peek-chk]')];
    const allChecked = boxes.length > 0 && state.peekSelectedRels.size === boxes.length;
    state.peekSelectedRels.clear();
    boxes.forEach(b => {
      b.checked = !allChecked;
      if (!allChecked) state.peekSelectedRels.add(b.dataset.peekChk);
    });
    const n = state.peekSelectedRels.size;
    $('peek-sel-count').textContent = n > 0 ? `${n} arquivo(s) marcado(s)` : `Todos os ${boxes.length} arquivo(s)`;
  };
  $('btn-peek-download').onclick = async () => {
    if (!state.lastPeekSource) return;
    const dest = prompt('Pasta de destino para salvar os arquivos descompilados (deixe vazio para Downloads/BrightVault-Restaurados):', '');
    if (dest === null) return;
    try {
      await api('/api/peek/extract', {
        method: 'POST',
        body: JSON.stringify({
          source: state.lastPeekSource,
          selected_rel_paths: [...state.peekSelectedRels],
          dest_dir: dest,
          skip_unchanged_local: $('peek-skip-unchanged').checked
        })
      });
      if ($('up-video-title')) $('up-video-title').value = '';
      closeModals();
      $('jobs-drawer').classList.remove('hidden');
      pollJobs();
      toast('⬇️ Baixando e descompilando os arquivos selecionados!');
    } catch (err) { toast('❌ ' + err.message, 5000); }
  };

  // Incremental Sync Modal (Delta Update Videos, max 10 updates per folder chain!)
  $('btn-open-sync').onclick = () => {
    $('sync-res').value = state.vault.settings.default_resolution || '4k';
    $('sync-prof').value = state.vault.settings.default_profile || 'y8-gray';
    $('sync-dest-folder').value = state.filters.folder_id || 'root';
    $('sync-diff-box').classList.add('hidden');
    $('sync-actions-box').classList.add('hidden');
    const groups = state.vault.sync_groups || [];
    if (groups.length) {
      $('sync-existing-wrap').classList.remove('hidden');
      $('sync-groups-list').innerHTML = groups.map(g =>
        `<button class="pill" data-sync-pick-id="${esc(g.id)}" title="${esc(g.local_path)}">🔄 ${esc(g.name)} (${esc(g.id)} · ${g.delta_count}/${g.max_deltas || 10})</button>`
      ).join('');
    } else {
      $('sync-existing-wrap').classList.add('hidden');
    }
    openModal('modal-sync');
  };
  $('sync-groups-list').onclick = (e) => {
    const b = e.target.closest('[data-sync-pick-id]');
    if (!b) return;
    const g = (state.vault.sync_groups || []).find(x => x.id === b.dataset.syncPickId);
    if (!g) return;
    $('sync-local-path').value = g.local_path;
    $('sync-dest-folder').value = g.folder_id || 'root';
    $('btn-sync-analyze').click();
  };
  $('btn-sync-pick').onclick = async () => {
    const res = await api('/api/system/pick-folder', { method: 'POST' });
    if (!res.cancelled && res.path) {
      $('sync-local-path').value = res.path;
      $('btn-sync-analyze').click();
    }
  };
  $('btn-sync-analyze').onclick = async () => {
    const box = $('sync-diff-box');
    box.classList.remove('hidden');
    box.textContent = '🔍 Comparando arquivos locais com o catálogo (data, tamanho e SHA-256)...';
    try {
      const diff = await api('/api/sync/analyze', {
        method: 'POST',
        body: JSON.stringify({ local_path: $('sync-local-path').value, folder_id: $('sync-dest-folder').value })
      });
      state.lastSyncAnalysis = diff;
      const chainBadge = diff.max_deltas === 0
        ? '<b>📦 Modo Somente Vídeos Inteiros (limite = 0 — sem vídeos de atualização)</b>'
        : diff.is_first_backup
          ? `<b>Primeiro Backup Base (0/${diff.max_deltas})</b>`
          : diff.needs_consolidation
            ? `<span style="color:var(--warn)"><b>⚠️ Limite atingido (${diff.delta_count}/${diff.max_deltas} atualizações) — Consolide um novo Backup Base!</b></span>`
            : `<b>Vídeo de Atualização ${diff.next_delta_index}/${diff.max_deltas}</b>`;
      box.innerHTML = `
        <div><b>Código da Pasta:</b> <code>${esc(diff.sync_id)}</code> · ${chainBadge}</div>
        <div style="margin-top:6px">
          ➕ <b>${diff.added.length}</b> novo(s) ·
          ✏️ <b>${diff.modified.length}</b> modificado(s) ·
          🗑️ <b>${diff.deleted.length}</b> removido(s) ·
          ✅ <b>${diff.unchanged_count}</b> igual(is) pulado(s)
        </div>
        <div class="muted sm-txt" style="margin-top:4px;margin-bottom:0">
          Volume a enviar: <b>${fmtBytes(diff.bytes_to_upload)}</b> (economizados <b>${fmtBytes(diff.bytes_skipped)}</b> que já estão no YouTube!)
        </div>`;
      $('sync-consolidate').checked = diff.needs_consolidation || diff.max_deltas === 0;
      $('sync-actions-box').classList.remove('hidden');
    } catch (err) {
      box.innerHTML = `<div class="alert error">${esc(err.message)}</div>`;
      $('sync-actions-box').classList.add('hidden');
    }
  };
  $('btn-sync-run').onclick = async () => {
    try {
      const res = await api('/api/sync/run', {
        method: 'POST',
        body: JSON.stringify({
          local_path: $('sync-local-path').value,
          folder_id: $('sync-dest-folder').value,
          sync_id: state.lastSyncAnalysis?.sync_id || '',
          resolution: $('sync-res').value,
          profile: $('sync-prof').value,
          auto_upload_yt: $('sync-auto-yt').checked,
          apply_deletes: $('sync-apply-deletes').checked,
          consolidate: $('sync-consolidate').checked
        })
      });
      closeModals();
      if (res.no_video_needed) {
        toast('✅ ' + (res.message || `${res.removed_files} arquivo(s) removido(s) sincronizado(s)!`));
        await refreshVault();
      } else {
        $('jobs-drawer').classList.remove('hidden');
        pollJobs();
        toast('🚀 Sincronização incremental iniciada!');
      }
    } catch (err) { toast('❌ ' + err.message, 6000); }
  };

  // Catalog Backup modal
  $('btn-open-catalog').onclick = () => {
    $('catalog-info').innerHTML = `<b>Sequência atual:</b> v${state.vault.catalog_seq} · <b>Último Vídeo-Índice:</b> ${state.vault.last_catalog_vid ? `<code>${esc(state.vault.last_catalog_vid)}</code>` : 'Nenhum ainda'}<br><b>YouTube API:</b> ${state.vault.youtube_connected ? '✅ Conectada (' + esc(state.vault.youtube_channel) + ')' : '⚠️ Não conectada (gera vídeo local para upload manual)'}`;
    openModal('modal-catalog');
  };
  $('btn-run-cat-backup').onclick = async () => {
    try {
      await api('/api/catalog/backup', {
        method: 'POST',
        body: JSON.stringify({
          password: $('cat-password').value,
          include_thumbs: $('cat-thumbs').checked,
          upload_youtube: $('cat-upload-yt').checked,
          delete_old_yt: $('cat-delete-old').checked
        })
      });
      closeModals(); $('jobs-drawer').classList.remove('hidden'); pollJobs();
    } catch (err) { toast('❌ ' + err.message, 5000); }
  };
  $('btn-export-ytcat').onclick = async () => {
    const pass = $('cat-password').value;
    if (!pass) return toast('Digite sua senha mestra.');
    const r = await fetch(`/api/catalog/export-file?thumbs=${$('cat-thumbs').checked}`, {
      credentials: 'same-origin', headers: { 'X-Vault-Password': pass }
    });
    if (!r.ok) return toast('Erro ao exportar .ytcat');
    const blob = await r.blob();
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = 'brightvault-catalogo.ytcat';
    a.click();
    toast('💾 Snapshot .ytcat baixado!');
  };

  // Floating Performance Panel (Main Screen: CPU %, RAM %, Priority, Quick Presets)
  const PERF_PRESETS = {
    max:      { cpu: 100, ram: 80, bg: false, label: 'Máximo' },
    balanced: { cpu: 50,  ram: 45, bg: false, label: 'Equilibrado' },
    min:      { cpu: 25,  ram: 20, bg: true,  label: 'Mínimo' },
  };
  const getSystemHW = () => ({
    cores: state.status?.system_info?.cpu_cores || 8,
    ramMB: state.status?.system_info?.total_ram_mb || 8192,
  });
  const fmtRAMShort = (mb) => mb >= 1024 ? `${(mb / 1024).toFixed(1)} GB` : `${mb} MB`;

  const syncPerfUI = () => {
    const hw = getSystemHW();
    const cpuPct = parseInt($('perf-cpu-pct')?.value || '50', 10);
    const ramPct = parseInt($('perf-ram-pct')?.value || '45', 10);
    const threads = Math.max(1, Math.min(hw.cores, Math.round((hw.cores * cpuPct) / 100)));
    const ramMB = Math.max(256, Math.round((hw.ramMB * ramPct) / 100));
    if ($('perf-hw-subtitle')) {
      $('perf-hw-subtitle').textContent = `PC detectado: ${hw.cores} threads · ${fmtRAMShort(hw.ramMB)} RAM`;
    }
    if ($('perf-cpu-val')) $('perf-cpu-val').textContent = `${cpuPct}% (${threads}/${hw.cores} threads)`;
    if ($('perf-ram-val')) $('perf-ram-val').textContent = `${ramPct}% (~${fmtRAMShort(ramMB)})`;
    const bg = !!$('perf-bg-mode')?.checked;
    let activePreset = 'custom';
    for (const [k, p] of Object.entries(PERF_PRESETS)) {
      if (cpuPct === p.cpu && ramPct === p.ram && bg === p.bg) {
        activePreset = k;
        break;
      }
    }
    document.querySelectorAll('[data-perf-preset]').forEach(b => {
      b.classList.toggle('active', b.dataset.perfPreset === activePreset);
    });
    if ($('perf-live-badge')) {
      const pName = PERF_PRESETS[activePreset]?.label || 'Personalizado';
      $('perf-live-badge').textContent = `${pName} · ${threads} threads · ${fmtRAMShort(ramMB)}`;
    }
    if ($('perf-pill-label')) {
      const pName = PERF_PRESETS[activePreset]?.label || `${cpuPct}%`;
      $('perf-pill-label').textContent = pName;
    }
    return { activePreset, cpuPct, ramPct, threads, ramMB };
  };

  const hydratePerfPanelFromVault = () => {
    const s = state.vault?.settings;
    if (!s) return;
    const hw = getSystemHW();
    const cpuPct = s.max_cpu_percent >= 10
      ? s.max_cpu_percent
      : Math.max(10, Math.min(100, Math.round(((s.cpu_threads || 2) / hw.cores) * 100 / 5) * 5));
    const ramPct = s.max_ram_percent >= 10
      ? s.max_ram_percent
      : Math.max(10, Math.min(85, Math.round(((s.max_ram_mb || 512) / hw.ramMB) * 100 / 5) * 5));
    if ($('perf-cpu-pct')) $('perf-cpu-pct').value = String(cpuPct);
    if ($('perf-ram-pct')) $('perf-ram-pct').value = String(ramPct);
    if ($('perf-bg-mode')) $('perf-bg-mode').checked = s.background_mode !== false;
    if ($('perf-opt-png')) $('perf-opt-png').checked = !!s.optimize_png_lossless;
    if ($('perf-cleanup')) $('perf-cleanup').value = s.cleanup_policy || 'safe_after_verify';
    syncPerfUI();
  };

  const applyPerformanceNow = async (presetOverride = null) => {
    const st = syncPerfUI();
    const preset = presetOverride || st.activePreset;
    try {
      const res = await api('/api/performance', {
        method: 'POST',
        body: JSON.stringify({
          perf_preset: preset,
          max_cpu_percent: st.cpuPct,
          max_ram_percent: st.ramPct,
          background_mode: !!$('perf-bg-mode')?.checked,
          optimize_png_lossless: !!$('perf-opt-png')?.checked,
          cleanup_policy: $('perf-cleanup')?.value || 'safe_after_verify',
        })
      });
      await refreshVault();
      hydratePerfPanelFromVault();
      toast(`⚡ Desempenho aplicado: ${res.cpu_threads} threads (${res.max_cpu_percent}% CPU) · ${fmtRAMShort(res.max_ram_mb)} RAM!`);
    } catch (err) { toast('❌ ' + err.message, 5000); }
  };

  const togglePerfPanel = (forceOpen = null) => {
    const panel = $('perf-floating-panel');
    const btn = $('btn-toggle-perf');
    if (!panel) return;
    const willOpen = forceOpen !== null ? forceOpen : panel.classList.contains('hidden');
    if (willOpen) hydratePerfPanelFromVault();
    panel.classList.toggle('hidden', !willOpen);
    if (btn) btn.classList.toggle('open', willOpen);
  };
  if ($('btn-toggle-perf')) $('btn-toggle-perf').onclick = (e) => { e.stopPropagation(); togglePerfPanel(); };
  if ($('btn-close-perf')) $('btn-close-perf').onclick = () => togglePerfPanel(false);
  if ($('btn-settings-open-perf')) $('btn-settings-open-perf').onclick = () => { closeModal('modal-settings'); togglePerfPanel(true); };
  document.querySelectorAll('[data-perf-preset]').forEach(b => {
    b.onclick = () => {
      const p = PERF_PRESETS[b.dataset.perfPreset];
      if (!p) return;
      $('perf-cpu-pct').value = String(p.cpu);
      $('perf-ram-pct').value = String(p.ram);
      $('perf-bg-mode').checked = p.bg;
      syncPerfUI();
      applyPerformanceNow(b.dataset.perfPreset);
    };
  });
  if ($('perf-cpu-pct')) $('perf-cpu-pct').oninput = syncPerfUI;
  if ($('perf-ram-pct')) $('perf-ram-pct').oninput = syncPerfUI;
  if ($('perf-bg-mode')) $('perf-bg-mode').onchange = syncPerfUI;
  if ($('btn-save-perf')) $('btn-save-perf').onclick = () => applyPerformanceNow();
  document.addEventListener('click', (e) => {
    const panel = $('perf-floating-panel');
    if (!panel || panel.classList.contains('hidden')) return;
    if (!panel.contains(e.target) && e.target !== $('btn-toggle-perf') && !$('btn-toggle-perf')?.contains(e.target)) {
      togglePerfPanel(false);
    }
  });
  window._hydratePerfPanel = hydratePerfPanelFromVault;

  // Settings modal
  if ($('set-theme')) {
    $('set-theme').onchange = () => {
      if (window.BV_THEME) window.BV_THEME.apply($('set-theme').value);
    };
  }
  if ($('set-language')) {
    $('set-language').onchange = () => {
      if (window.BV_I18N) window.BV_I18N.apply($('set-language').value);
    };
  }
  $('btn-open-settings').onclick = () => {
    const s = state.vault.settings;
    if ($('set-theme')) $('set-theme').value = s.theme || (window.BV_THEME?.current() || 'bright-night');
    if ($('set-language')) $('set-language').value = s.language || (window.BV_I18N?.current() || 'pt');
    $('set-res').value = validResCodes.has(s.default_resolution) ? s.default_resolution : '2k';
    $('set-prof').value = s.default_profile || 'y8-gray';
    $('set-vcodec').value = normalizeCodecSelect(s.default_video_codec);
    updateProfileDesc('set-prof', 'set-prof-desc');
    updateSettingsAttrs();
    $('set-cleanup').value = s.cleanup_policy || 'safe_after_verify';
    $('set-privacy').value = s.default_privacy || 'unlisted';
    $('set-max-deltas').value = String(s.max_delta_videos ?? 10);
    $('set-keep-prev').checked = !!s.catalog_keep_previous;
    $('set-opt-png').checked = !!s.optimize_png_lossless;
    const usesKF = !!state.status?.vault_uses_keyfile;
    const kfHint = state.status?.vault_keyfile_hint || '';
    $('sec-cur-kf-wrap').classList.toggle('hidden', !usesKF);
    if (usesKF) {
      $('sec-cur-kf-label').textContent = `Arquivo-Chave Atual (${kfHint || 'obrigatório'})`;
    }
    $('sec-pq-status').innerHTML = `🛡️ <b>Criptografia Pós-Quântica em Cascata ativa:</b> <code>Argon2id</code> + <code>HKDF-SHA512</code> + <b>AES-256-GCM + XChaCha20-Poly1305</b>.<br>🔑 <b>2º Fator (Arquivo-Chave):</b> ${usesKF ? `<span style="color:#4ade80">ATIVO (${esc(kfHint || 'configurado')})</span>` : '<span class="muted">Não configurado (opcional — adicione abaixo para proteção anti-IA)</span>'}`;
    $('sec-remove-kf').checked = false;
    $('set-yt-cid').value = s.youtube_client_id || '';
    $('set-yt-csec').value = s.youtube_client_secret || '';
    $('set-yt-cookies-browser').value = s.ytdlp_cookies_browser || '';
    $('set-yt-cookies-file').value = s.ytdlp_cookies_file || '';
    $('yt-js-status').textContent = state.status?.tools?.has_js_runtime
      ? `✅ Motor JS detectado para 4K/8K no yt-dlp: ${state.status.tools.js_runtime}`
      : '⚠️ Nenhum runtime JS (Node.js/Deno) detectado — yt-dlp pode ficar limitado a 360p.';
    $('yt-conn-status').textContent = state.vault.youtube_connected
      ? `✅ Conectado ao canal: ${state.vault.youtube_channel}`
      : 'Não conectado (o Modo Manual Assistido funciona sem API).';
    openModal('modal-settings');
  };
  document.querySelectorAll('[data-set-tab]').forEach(b => b.onclick = () => {
    const t = b.dataset.setTab;
    document.querySelectorAll('[data-set-tab]').forEach(x => x.classList.toggle('active', x === b));
    ['general', 'youtube', 'calibrate', 'security'].forEach(k => $(`set-tab-${k}`).classList.toggle('hidden', k !== t));
  });

  const saveSettings = async () => {
    const cur = state.vault?.settings || {};
    await api('/api/settings', {
      method: 'POST',
      body: JSON.stringify({
        default_resolution: $('set-res').value,
        default_profile: $('set-prof').value,
        default_video_codec: $('set-vcodec').value,
        max_ram_mb: cur.max_ram_mb || 512,
        cpu_threads: cur.cpu_threads || 2,
        max_cpu_percent: cur.max_cpu_percent || 0,
        max_ram_percent: cur.max_ram_percent || 0,
        perf_preset: cur.perf_preset || '',
        background_mode: cur.background_mode !== false,
        cleanup_policy: $('set-cleanup').value,
        default_privacy: $('set-privacy').value,
        theme: $('set-theme')?.value || 'bright-night',
        language: $('set-language')?.value || 'pt',
        max_delta_videos: Math.max(0, Math.min(99, parseInt($('set-max-deltas').value, 10) || 0)),
        ytdlp_cookies_browser: $('set-yt-cookies-browser').value,
        ytdlp_cookies_file: $('set-yt-cookies-file').value,
        catalog_keep_previous: $('set-keep-prev').checked,
        optimize_png_lossless: $('set-opt-png').checked,
        youtube_client_id: $('set-yt-cid').value,
        youtube_client_secret: $('set-yt-csec').value
      })
    });
    toast('✅ Configurações salvas!');
    await refreshVault();
  };
  $('btn-reset-deltas-rec').onclick = () => {
    $('set-max-deltas').value = '10';
    toast('⭐ Limite restaurado para a configuração recomendada (10 vídeos de atualização).');
  };
  $('btn-open-oauth-tutorial').onclick = () => openModal('modal-oauth-tutorial');
  $('btn-save-settings').onclick = saveSettings;
  $('btn-save-yt-creds').onclick = saveSettings;
  $('btn-connect-yt').onclick = async () => {
    try {
      await saveSettings();
      await api('/api/youtube/oauth/start', { method: 'POST' });
      $('jobs-drawer').classList.remove('hidden'); pollJobs();
    } catch (err) { toast('❌ ' + err.message, 5000); }
  };
  if ($('btn-reconnect-yt')) {
    $('btn-reconnect-yt').onclick = async () => {
      try {
        await saveSettings();
        await api('/api/youtube/oauth/reconnect', { method: 'POST' });
        $('jobs-drawer').classList.remove('hidden'); pollJobs();
        toast('🔄 Token antigo limpo! Conclua o login na janela do Google para reconectar o YouTube.', 5500);
      } catch (err) { toast('❌ ' + err.message, 6000); }
    };
  }
  if ($('bv-update-btn')) {
    $('bv-update-btn').onclick = async () => {
      const btn = $('bv-update-btn');
      const u = state.updateStatus;
      if (u && u.update_available && u.latest_version) {
        if (!confirm(`Atualizar BrightVault automaticamente de v${u.current_version || '0.701'} para v${u.latest_version}?\n\nO programa irá baixar a nova versão do GitHub e reiniciar automaticamente.`)) return;
        btn.disabled = true;
        btn.textContent = '⏳ Atualizando...';
        try {
          const res = await api('/api/update/apply', { method: 'POST' });
          toast(`✅ ${res.message || 'Atualização iniciada! Reiniciando...'}`, 8000);
        } catch (err) {
          toast('❌ Falha na atualização automática: ' + err.message, 7000);
          renderUpdatePill(state.updateStatus);
        } finally {
          btn.disabled = false;
        }
        return;
      }
      btn.disabled = true;
      btn.textContent = '🔍 Verificando...';
      try {
        const res = await api('/api/update/check', { method: 'POST' });
        renderUpdatePill(res);
        if (res.error) {
          toast(`⚠️ Verificação de atualização: ${res.error}`, 5500);
        } else if (res.update_available) {
          toast(`⬆️ Nova versão v${res.latest_version} disponível! Clique no indicador para atualizar.`, 6500);
        } else {
          toast(`✅ BrightVault v${res.current_version || '0.701'} está na versão mais recente!`, 4000);
        }
      } catch (err) {
        toast('❌ ' + err.message, 5000);
        renderUpdatePill(state.updateStatus);
      } finally {
        btn.disabled = false;
      }
    };
  }

  $('btn-run-calibrate').onclick = async () => {
    $('cal-results').innerHTML = '<div class="muted sm-pad">Rodando calibração óptica via FFmpeg...</div>';
    await api('/api/calibrate', {
      method: 'POST',
      body: JSON.stringify({ resolution: $('cal-res').value, crf: parseInt($('cal-crf').value, 10) || 26 })
    });
    $('jobs-drawer').classList.remove('hidden'); pollJobs();
  };

  $('btn-change-pass').onclick = async () => {
    try {
      await api('/api/vault/password', {
        method: 'POST',
        body: JSON.stringify({
          current_password: $('sec-cur-pass').value,
          current_keyfile_path: $('sec-cur-kf-path').value.trim(),
          new_password: $('sec-new-pass').value,
          new_keyfile_path: $('sec-new-kf-path').value.trim(),
          remove_keyfile: $('sec-remove-kf').checked
        })
      });
      $('sec-cur-pass').value = ''; $('sec-new-pass').value = '';
      $('sec-cur-kf-path').value = ''; $('sec-new-kf-path').value = '';
      $('sec-remove-kf').checked = false;
      await refreshVault();
      toast('🔑 Senha mestra e Arquivo-Chave atualizados com sucesso!');
    } catch (err) { toast('❌ ' + err.message, 5000); }
  };

  $('btn-sec-forget-pass').onclick = async () => {
    await api('/api/vault/forget-password', { method: 'POST' });
    toast('🗑️ Senha salva removida deste computador.');
  };

  $('btn-install-tools').onclick = async () => {
    await api('/api/tools/install', { method: 'POST' });
    $('jobs-drawer').classList.remove('hidden');
    pollJobs();
  };
  $('btn-toggle-jobs').onclick = () => $('jobs-drawer').classList.add('hidden');
}

init();
