(() => {
  const $ = (id) => document.getElementById(id);
  const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const fmtBytes = (b) => {
    if (!b || b < 1024) return `${b || 0} B`;
    if (b < 1048576) return `${(b / 1024).toFixed(1)} KB`;
    if (b < 1073741824) return `${(b / 1048576).toFixed(2)} MB`;
    return `${(b / 1073741824).toFixed(2)} GB`;
  };

  let toastTimer = null;
  const toast = (msg, ms = 3500) => {
    const el = $('toast');
    el.textContent = msg;
    el.classList.remove('hidden');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => el.classList.add('hidden'), ms);
  };

  async function api(url, opts = {}) {
    const res = await fetch(url, {
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', ...(opts.headers || {}) },
      ...opts,
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`);
    return data;
  }

  const STORAGE_BM = 'brightweb_bookmarks_v1';
  const STORAGE_HIST = 'brightweb_history_v1';
  const loadList = (k) => { try { return JSON.parse(localStorage.getItem(k) || '[]'); } catch { return []; } };
  const saveList = (k, arr) => { try { localStorage.setItem(k, JSON.stringify(arr.slice(0, 60))); } catch { } };
  const persistPrefs = (patch) => {
    api('/api/prefs', { method: 'POST', body: JSON.stringify(patch) }).catch(() => { });
  };

  const state = {
    tabs: [],
    activeTabId: null,
    isolated: true,
    bookmarks: loadList(STORAGE_BM),
    history: loadList(STORAGE_HIST),
    nextTabNum: 1,
  };

  const getActiveTab = () => state.tabs.find((t) => t.id === state.activeTabId);

  function createTab(initialAddr = '') {
    const id = 'tab_' + (state.nextTabNum++);
    const tab = {
      id,
      title: 'Nova Aba',
      address: initialAddr,
      view: 'home', // 'home' | 'loading' | 'locked' | 'error' | 'site'
      site: null,
      subPath: '',
      history: [],
      histIdx: -1,
      lockLayers: 0,
      lockAddress: '',
      errorMsg: '',
      pollTimer: null,
    };
    state.tabs.push(tab);
    state.activeTabId = id;
    renderAll();
    if (initialAddr) navigateTab(tab, initialAddr);
    return tab;
  }

  function closeTab(id) {
    const idx = state.tabs.findIndex((t) => t.id === id);
    if (idx < 0) return;
    clearInterval(state.tabs[idx].pollTimer);
    state.tabs.splice(idx, 1);
    if (!state.tabs.length) {
      createTab();
      return;
    }
    if (state.activeTabId === id) {
      state.activeTabId = state.tabs[Math.max(0, idx - 1)].id;
    }
    renderAll();
  }

  function pushHistory(tab, addr) {
    if (tab.histIdx < tab.history.length - 1) {
      tab.history = tab.history.slice(0, tab.histIdx + 1);
    }
    if (tab.history[tab.history.length - 1] !== addr) {
      tab.history.push(addr);
      tab.histIdx = tab.history.length - 1;
    }
  }

  function recordGlobalHistory(title, address) {
    if (!address) return;
    state.history = [{ title: title || address, address, at: new Date().toISOString() }, ...state.history.filter((x) => x.address !== address)].slice(0, 60);
    saveList(STORAGE_HIST, state.history);
    persistPrefs({ history: state.history });
    renderHomeLists();
  }

  function renderTabs() {
    $('tabs-strip').innerHTML = state.tabs.map((t) => `
      <div class="b-tab ${t.id === state.activeTabId ? 'active' : ''}" data-tab-id="${t.id}">
        <span class="b-tab-title" title="${esc(t.title)}">${t.view === 'loading' ? '⏳ ' : t.view === 'locked' ? '🔒 ' : t.view === 'site' ? '🌐 ' : '🏠 '}${esc(t.title)}</span>
        <button type="button" class="b-tab-close" data-tab-close="${t.id}" title="Fechar aba">✕</button>
      </div>`).join('');
  }

  function renderNavBar() {
    const tab = getActiveTab();
    if (!tab) return;
    $('address-input').value = tab.address || '';
    $('btn-back').disabled = tab.histIdx <= 0;
    $('btn-fwd').disabled = tab.histIdx >= tab.history.length - 1;
    const isBM = !!(tab.address && state.bookmarks.some((b) => b.address === tab.address));
    $('btn-bookmark').textContent = isBM ? '★' : '☆';
    $('btn-bookmark').style.color = isBM ? '#fbbf24' : '';
    const badge = $('omni-badge');
    if (tab.view === 'site' && tab.site) {
      badge.textContent = tab.site.encryption_mode === 'public-pq' ? `🔒 PQ×${tab.site.password_layers}` : '☀️ btweb://';
    } else {
      badge.textContent = 'btweb://';
    }
  }

  function renderHomeLists() {
    const bmEl = $('home-bookmarks');
    bmEl.innerHTML = state.bookmarks.length
      ? state.bookmarks.map((b) => `
        <div class="link-item" data-open-addr="${esc(b.address)}">
          <div style="min-width:0"><div class="link-item-title">⭐ ${esc(b.title)}</div><div class="link-item-url">${esc(b.address)}</div></div>
        </div>`).join('')
      : '<p class="muted" style="font-size:12px">Nenhum favorito salvo ainda. Clique em ☆ na barra de endereço ao visitar um site.</p>';

    const hiEl = $('home-history');
    hiEl.innerHTML = state.history.length
      ? state.history.slice(0, 8).map((h) => `
        <div class="link-item" data-open-addr="${esc(h.address)}">
          <div style="min-width:0"><div class="link-item-title">🌐 ${esc(h.title)}</div><div class="link-item-url">${esc(h.address)}</div></div>
        </div>`).join('')
      : '<p class="muted" style="font-size:12px">Nenhum site visitado ainda. Experimente gerar o Site Demo em 1 clique!</p>';
  }

  function renderInspector() {
    const tab = getActiveTab();
    const body = $('insp-body');
    if (!tab || !tab.site) {
      body.innerHTML = '<p class="muted">Abra qualquer site óptico para inspecionar telemetria do codec, erros corrigidos pelo Reed-Solomon (FEC), criptografia e arquivos na RAM.</p>';
      return;
    }
    const s = tab.site;
    const encLbl = s.encryption_mode === 'public-pq' ? `Pós-Quântica (${s.password_layers}×)` : 'Web Aberta (YTWB001)';
    body.innerHTML = `
      <div class="insp-stat-grid">
        <div class="insp-stat"><span>Resolução</span><b>${esc(s.resolution)}</b></div>
        <div class="insp-stat"><span>Perfil Óptico</span><b>${esc(s.profile_code)}</b></div>
        ${s.shard_count > 1 ? `<div class="insp-stat"><span>Sharding Paralelo</span><b>🧩 ${s.shard_count} vídeos</b></div>` : ''}
        <div class="insp-stat"><span>Frames Lidos</span><b>${s.total_frames}</b></div>
        <div class="insp-stat"><span>Erros Corrigidos (FEC)</span><b>${s.corrected_bytes} B</b></div>
        <div class="insp-stat"><span>Tamanho Site (RAM)</span><b>${fmtBytes(s.total_bytes)}</b></div>
        <div class="insp-stat"><span>Criptografia</span><b>${esc(encLbl)}</b></div>
      </div>
      <div><b>Isolamento:</b> ${state.isolated ? '<span style="color:var(--ok)">🛡️ CSP Offline Ativo</span>' : '<span style="color:#fbbf24">⚠️ Conexões externas permitidas</span>'}</div>
      <div><b>Arquivos no Vídeo (${(s.files || []).length}):</b></div>
      <div style="display:flex;flex-direction:column;gap:5px">
        ${(s.files || []).map((f) => `
          <div class="insp-file" data-insp-path="${esc(f.path)}" title="SHA-256: ${esc(f.sha256)}...">
            <span style="overflow:hidden;text-overflow:ellipsis;white-space:nowrap">📄 ${esc(f.path)}</span>
            <span class="muted">${fmtBytes(f.size)}</span>
          </div>`).join('')}
      </div>`;
  }

  function renderViewport() {
    const tab = getActiveTab();
    if (!tab) return;
    $('view-home').classList.toggle('hidden', tab.view !== 'home');
    $('view-loading').classList.toggle('hidden', tab.view !== 'loading');
    $('view-locked').classList.toggle('hidden', tab.view !== 'locked');
    $('view-error').classList.toggle('hidden', tab.view !== 'error');
    const frame = $('site-frame');
    frame.classList.toggle('hidden', tab.view !== 'site');

    if (tab.view === 'site' && tab.site) {
      const targetUrl = `/site/${tab.site.site_id}/${tab.site.entry_path || ''}`;
      if (frame.dataset.loadedUrl !== targetUrl) {
        frame.dataset.loadedUrl = targetUrl;
        frame.src = targetUrl;
      }
    } else if (tab.view === 'locked') {
      $('lock-desc').textContent = `Este site óptico está protegido por ${tab.lockLayers} camada(s) independente(s) de criptografia Pós-Quântica (Argon2id + AES-256-GCM + XChaCha20-Poly1305).`;
      const list = $('lock-pw-list');
      list.innerHTML = Array.from({ length: tab.lockLayers }, (_, i) =>
        `<input type="password" class="lock-pw-in" placeholder="Senha ${i + 1} de ${tab.lockLayers}">`
      ).join('');
      $('lock-error').classList.add('hidden');
      setTimeout(() => list.querySelector('input')?.focus(), 50);
    } else if (tab.view === 'error') {
      $('err-detail').textContent = tab.errorMsg || 'Erro desconhecido';
    }
  }

  function renderAll() {
    renderTabs();
    renderNavBar();
    renderViewport();
    renderInspector();
  }

  async function navigateTab(tab, rawAddress, opts = {}) {
    const addr = (rawAddress || '').trim();
    if (!addr) return;
    clearInterval(tab.pollTimer);
    tab.address = addr;
    if (!opts.skipHistory) pushHistory(tab, addr);
    tab.view = 'loading';
    tab.title = 'Decodificando...';
    tab.errorMsg = '';
    if (tab.id === state.activeTabId) {
      $('load-title').textContent = 'Decodificando vídeo óptico...';
      $('load-detail').textContent = addr;
      $('load-bar').style.width = '6%';
      $('load-pct').textContent = '6%';
      $('load-extra').textContent = '';
    }
    renderAll();

    try {
      const resp = await api('/api/navigate', {
        method: 'POST',
        body: JSON.stringify({
          address: addr,
          passwords: opts.passwords || [],
          force_reload: !!opts.forceReload,
        }),
      });
      if (resp.cached && resp.site) {
        mountSiteOnTab(tab, resp.site);
        return;
      }
      const jobId = resp.job?.id;
      if (!jobId) throw new Error('Falha iniciando decodificação');

      const tick = async () => {
        try {
          const j = await api(`/api/jobs/${jobId}`);
          if (tab.id === state.activeTabId && tab.view === 'loading') {
            $('load-detail').textContent = j.detail || 'Processando...';
            $('load-bar').style.width = `${j.progress || 0}%`;
            $('load-pct').textContent = `${j.progress || 0}%`;
            $('load-extra').textContent = [j.speed, j.eta].filter(Boolean).join(' · ');
          }
          if (j.status === 'done' && j.result) {
            clearInterval(tab.pollTimer);
            mountSiteOnTab(tab, j.result);
          } else if (j.status === 'needs_password') {
            clearInterval(tab.pollTimer);
            tab.view = 'locked';
            tab.title = '🔒 Site Protegido';
            tab.lockLayers = j.password_layers || 1;
            tab.lockAddress = addr;
            renderAll();
          } else if (j.status === 'error') {
            clearInterval(tab.pollTimer);
            if (j.password_layers > 0 && (opts.passwords || []).length > 0) {
              // Wrong password on locked site -> stay on lock screen with error!
              tab.view = 'locked';
              tab.lockLayers = j.password_layers;
              tab.lockAddress = addr;
              renderAll();
              $('lock-error').textContent = '❌ ' + (j.error || 'Senha incorreta');
              $('lock-error').classList.remove('hidden');
            } else {
              tab.view = 'error';
              tab.title = 'Erro ao abrir';
              tab.errorMsg = j.error || 'Erro desconhecido';
              renderAll();
            }
          }
        } catch (err) {
          clearInterval(tab.pollTimer);
          tab.view = 'error';
          tab.title = 'Erro';
          tab.errorMsg = err.message;
          renderAll();
        }
      };
      tab.pollTimer = setInterval(tick, 450);
      tick();
    } catch (err) {
      tab.view = 'error';
      tab.title = 'Erro';
      tab.errorMsg = err.message;
      renderAll();
    }
  }

  function mountSiteOnTab(tab, site) {
    tab.site = site;
    tab.view = 'site';
    tab.title = site.title || site.display_address || 'Site Óptico';
    tab.address = site.display_address || tab.address;
    // Force iframe reload cleanly
    $('site-frame').dataset.loadedUrl = '';
    recordGlobalHistory(tab.title, tab.address);
    renderAll();
  }

  // Receive btweb:// (and legacy ytweb://) navigation clicks & document.title updates from sandboxed site iframe!
  window.addEventListener('message', (e) => {
    const msg = e.data;
    if (!msg || typeof msg !== 'object') return;
    const tab = getActiveTab();
    if (!tab) return;
    if ((msg.type === 'btweb-navigate' || msg.type === 'ytweb-navigate') && msg.url) {
      navigateTab(tab, msg.url);
    } else if ((msg.type === 'btweb-title' || msg.type === 'ytweb-title') && msg.title && tab.view === 'site') {
      tab.title = msg.title;
      renderTabs();
    }
  });

  // Toolbar & Tab Events
  $('btn-new-tab').onclick = () => createTab();
  $('tabs-strip').onclick = (e) => {
    const closeBtn = e.target.closest('[data-tab-close]');
    if (closeBtn) { closeTab(closeBtn.dataset.tabClose); return; }
    const tabEl = e.target.closest('[data-tab-id]');
    if (tabEl) {
      state.activeTabId = tabEl.dataset.tabId;
      $('site-frame').dataset.loadedUrl = '';
      renderAll();
    }
  };

  $('address-form').onsubmit = (e) => {
    e.preventDefault();
    const tab = getActiveTab();
    if (tab) navigateTab(tab, $('address-input').value);
  };
  $('btn-back').onclick = () => {
    const tab = getActiveTab();
    if (!tab || tab.histIdx <= 0) return;
    tab.histIdx--;
    navigateTab(tab, tab.history[tab.histIdx], { skipHistory: true });
  };
  $('btn-fwd').onclick = () => {
    const tab = getActiveTab();
    if (!tab || tab.histIdx >= tab.history.length - 1) return;
    tab.histIdx++;
    navigateTab(tab, tab.history[tab.histIdx], { skipHistory: true });
  };
  $('btn-reload').onclick = () => {
    const tab = getActiveTab();
    if (tab && tab.address) navigateTab(tab, tab.address, { skipHistory: true, forceReload: true });
  };
  $('btn-home').onclick = () => {
    const tab = getActiveTab();
    if (!tab) return;
    clearInterval(tab.pollTimer);
    tab.view = 'home';
    tab.title = 'Nova Aba';
    tab.address = '';
    tab.site = null;
    renderAll();
  };

  const openLocalVideo = async () => {
    try {
      const res = await api('/api/system/pick-video', { method: 'POST' });
      if (!res.cancelled && (res.path || (res.paths && res.paths.length))) {
        const tab = getActiveTab() || createTab();
        const target = (res.paths && res.paths.length > 1) ? res.paths.join(' | ') : res.path;
        navigateTab(tab, target);
      }
    } catch (err) { toast('❌ ' + err.message, 5000); }
  };
  $('btn-open-local').onclick = openLocalVideo;
  $('btn-home-local').onclick = openLocalVideo;

  $('btn-bookmark').onclick = () => {
    const tab = getActiveTab();
    if (!tab || !tab.address) return toast('Abra um site primeiro para salvar nos favoritos.');
    const exists = state.bookmarks.some((b) => b.address === tab.address);
    if (exists) {
      state.bookmarks = state.bookmarks.filter((b) => b.address !== tab.address);
      toast('Removido dos favoritos.');
    } else {
      state.bookmarks.unshift({ title: tab.title || tab.address, address: tab.address });
      toast('⭐ Salvo nos favoritos!');
    }
    saveList(STORAGE_BM, state.bookmarks);
    persistPrefs({ bookmarks: state.bookmarks });
    renderNavBar();
    renderHomeLists();
  };

  $('btn-isolated').onclick = async () => {
    state.isolated = !state.isolated;
    try {
      await api('/api/settings/isolated', { method: 'POST', body: JSON.stringify({ isolated: state.isolated }) });
      $('btn-isolated').classList.toggle('active', state.isolated);
      $('btn-isolated').textContent = state.isolated ? '🛡️ Isolado' : '⚠️ Aberto';
      toast(state.isolated ? '🛡️ Modo Isolado (CSP Offline) ativado!' : '⚠️ Modo Isolado desativado (site pode acessar recursos externos).');
      renderInspector();
      const tab = getActiveTab();
      if (tab && tab.view === 'site') {
        $('site-frame').dataset.loadedUrl = '';
        renderViewport();
      }
    } catch (err) { toast('❌ ' + err.message); }
  };

  $('btn-inspector').onclick = () => {
    $('inspector-panel').classList.toggle('hidden');
    renderInspector();
  };
  $('btn-close-inspector').onclick = () => $('inspector-panel').classList.add('hidden');
  $('insp-body').onclick = (e) => {
    const f = e.target.closest('[data-insp-path]');
    const tab = getActiveTab();
    if (!f || !tab || !tab.site) return;
    const url = `/site/${tab.site.site_id}/${f.dataset.inspPath}`;
    $('site-frame').dataset.loadedUrl = url;
    $('site-frame').src = url;
  };

  // Lock screen actions
  $('btn-lock-toggle').onclick = () => {
    const inputs = [...document.querySelectorAll('.lock-pw-in')];
    if (!inputs.length) return;
    const next = inputs[0].type === 'password' ? 'text' : 'password';
    inputs.forEach((i) => { i.type = next; });
  };
  $('btn-lock-unlock').onclick = () => {
    const tab = getActiveTab();
    if (!tab) return;
    const pws = [...document.querySelectorAll('.lock-pw-in')].map((i) => i.value);
    if (pws.some((p) => !p)) {
      $('lock-error').textContent = 'Preencha todas as camadas de senha.';
      $('lock-error').classList.remove('hidden');
      return;
    }
    navigateTab(tab, tab.lockAddress || tab.address, { passwords: pws, skipHistory: true });
  };

  $('btn-err-home').onclick = () => $('btn-home').click();
  $('btn-err-retry').onclick = () => $('btn-reload').click();

  // Home page quick links & 1-click Demo Site
  $('view-home').onclick = (e) => {
    const item = e.target.closest('[data-open-addr]');
    if (!item) return;
    const tab = getActiveTab() || createTab();
    navigateTab(tab, item.dataset.openAddr);
  };

  $('btn-home-demo').onclick = async () => {
    const tab = getActiveTab() || createTab();
    tab.view = 'loading';
    tab.title = 'Gerando Demo...';
    $('load-title').textContent = 'Gerando Site Demo em Vídeo Fractal (Julia)...';
    $('load-detail').textContent = 'Compactando HTML/CSS/JS e renderizando MP4 óptico...';
    $('load-bar').style.width = '10%';
    $('load-pct').textContent = '10%';
    renderAll();

    try {
      const job = await api('/api/publish/demo', { method: 'POST', body: JSON.stringify({}) });
      const timer = setInterval(async () => {
        try {
          const j = await api(`/api/jobs/${job.id}`);
          $('load-detail').textContent = j.detail || 'Renderizando...';
          $('load-bar').style.width = `${j.progress || 0}%`;
          $('load-pct').textContent = `${j.progress || 0}%`;
          if (j.status === 'done' && j.result?.output_path) {
            clearInterval(timer);
            toast(`✅ Vídeo Demo criado (${fmtBytes(j.result.video_size_bytes)}) — abrindo no BrightWeb!`, 4500);
            navigateTab(tab, j.result.output_path);
          } else if (j.status === 'error') {
            clearInterval(timer);
            tab.view = 'error';
            tab.errorMsg = j.error || 'Erro gerando demo';
            renderAll();
          }
        } catch (err) { clearInterval(timer); }
      }, 450);
    } catch (err) {
      tab.view = 'error';
      tab.errorMsg = err.message;
      renderAll();
    }
  };

  // BrightWeb Appearance & Language modal (6 themes + 9 languages)
  if ($('btn-bw-settings')) {
    $('btn-bw-settings').onclick = () => {
      if ($('bw-theme-select') && window.BV_THEME) $('bw-theme-select').value = window.BV_THEME.current();
      if ($('bw-lang-select') && window.BV_I18N) $('bw-lang-select').value = window.BV_I18N.current();
      $('modal-bw-settings').classList.remove('hidden');
    };
  }
  const closeBWSettings = () => $('modal-bw-settings')?.classList.add('hidden');
  if ($('btn-close-bw-settings')) $('btn-close-bw-settings').onclick = closeBWSettings;
  if ($('btn-done-bw-settings')) $('btn-done-bw-settings').onclick = closeBWSettings;
  if ($('bw-theme-select')) {
    $('bw-theme-select').onchange = () => {
      const th = $('bw-theme-select').value;
      if (window.BV_THEME) window.BV_THEME.apply(th);
      persistPrefs({ theme: th });
    };
  }
  if ($('bw-lang-select')) {
    $('bw-lang-select').onchange = () => {
      const lg = $('bw-lang-select').value;
      if (window.BV_I18N) window.BV_I18N.apply(lg);
      persistPrefs({ language: lg });
    };
  }

  // GitHub Auto-Update Badge (BrightWeb v0.7)
  state.updateInfo = null;
  const renderUpdatePill = (u) => {
    state.updateInfo = u;
    const btn = $('bw-update-btn');
    if (!btn || !u) return;
    const cur = u.current_version || '0.7';
    if (u.update_available && u.latest_version) {
      btn.classList.add('outdated');
      btn.textContent = `⬆️ Atualizar v${u.latest_version} (atual v${cur})`;
      btn.title = `Nova versão v${u.latest_version} disponível no GitHub! Clique para atualizar automaticamente.`;
    } else {
      btn.classList.remove('outdated');
      btn.textContent = `v${cur}`;
      btn.title = `BrightWeb v${cur} atualizado · Clique para verificar atualizações no GitHub`;
    }
  };
  if ($('bw-update-btn')) {
    $('bw-update-btn').onclick = async () => {
      const btn = $('bw-update-btn');
      if (state.updateInfo?.update_available) {
        if (!confirm(`Atualizar BrightWeb da v${state.updateInfo.current_version} para a v${state.updateInfo.latest_version} automaticamente agora?`)) return;
        btn.disabled = true;
        btn.textContent = '⏳ Atualizando...';
        try {
          const res = await api('/api/update/apply', { method: 'POST' });
          if (res.opened_release_page) {
            toast(`🌐 Página da versão v${res.latest_version} aberta no navegador!`, 5000);
          } else if (res.restarting) {
            toast(`🚀 Atualização v${res.latest_version} baixada! Reiniciando BrightWeb...`, 6000);
          } else {
            toast(`✅ BrightWeb v${res.current_version} já está na versão mais recente!`);
          }
        } catch (err) {
          toast('❌ Erro atualizando: ' + err.message, 6000);
        } finally {
          btn.disabled = false;
          renderUpdatePill(state.updateInfo);
        }
        return;
      }
      btn.disabled = true;
      btn.textContent = '🔄 ...';
      try {
        const u = await api('/api/update/check', { method: 'POST' });
        renderUpdatePill(u);
        if (u.update_available) {
          toast(`⬆️ Nova versão v${u.latest_version} disponível! Clique no indicador para atualizar.`, 5500);
        } else {
          toast(`✅ BrightWeb v${u.current_version} já está atualizado!`);
        }
      } catch (err) {
        toast('⚠️ Não foi possível verificar atualizações: ' + err.message, 4500);
      } finally {
        btn.disabled = false;
      }
    };
  }

  // Publish Website Modal — Audio Controls
  const syncPubAudioUI = () => {
    const mode = $('pub-audio-mode')?.value || 'none';
    $('pub-music-wrap')?.classList.toggle('hidden', mode !== 'ambient');
    $('pub-audio-file-wrap')?.classList.toggle('hidden', mode !== 'file');
  };
  if ($('pub-audio-mode')) $('pub-audio-mode').onchange = syncPubAudioUI;
  if ($('btn-pub-pick-audio')) {
    $('btn-pub-pick-audio').onclick = async () => {
      try {
        const res = await api('/api/system/pick-audio', { method: 'POST' });
        if (!res.cancelled && res.path) $('pub-audio-source').value = res.path;
      } catch (err) { toast('❌ ' + err.message); }
    };
  }

  // Publish Website Modal
  const openPublishModal = () => {
    $('pub-progress').classList.add('hidden');
    $('pub-result').classList.add('hidden');
    $('modal-publish').classList.remove('hidden');
  };
  const closePublishModal = () => $('modal-publish').classList.add('hidden');
  $('btn-open-publish').onclick = openPublishModal;
  $('btn-home-publish').onclick = openPublishModal;
  $('btn-close-publish').onclick = closePublishModal;
  $('btn-cancel-publish').onclick = closePublishModal;

  $('pub-access').onchange = () => {
    $('pub-pw-wrap').classList.toggle('hidden', $('pub-access').value !== 'encrypted');
  };
  $('btn-pub-add-pw').onclick = () => {
    const list = $('pub-pw-list');
    const n = list.querySelectorAll('.pub-pw').length;
    if (n >= 5) return toast('Máximo de 5 senhas em cascata.');
    const inp = document.createElement('input');
    inp.type = 'password';
    inp.className = 'pub-pw';
    inp.placeholder = `Senha ${n + 1} (Camada extra Pós-Quântica)`;
    list.appendChild(inp);
  };
  $('btn-pub-pick-folder').onclick = async () => {
    try {
      const res = await api('/api/system/pick-folder', { method: 'POST' });
      if (!res.cancelled && res.path) {
        $('pub-folder').value = res.path;
        $('pub-use-demo').checked = false;
      }
    } catch (err) { toast('❌ ' + err.message); }
  };
  $('btn-pub-pick-host').onclick = async () => {
    try {
      const res = await api('/api/system/pick-video', { method: 'POST' });
      if (!res.cancelled && res.path) {
        $('pub-host-video').value = res.path;
        if ($('pub-profile').value.startsWith('stealth-') && $('pub-theme').querySelector('option[value="none"]')) {
          $('pub-theme').value = 'none';
        }
      }
    } catch (err) { toast('❌ ' + err.message); }
  };
  $('pub-profile').onchange = () => {
    if ($('pub-profile').value.startsWith('stealth-') && $('pub-host-video').value.trim() && $('pub-theme').querySelector('option[value="none"]')) {
      $('pub-theme').value = 'none';
    }
  };

  $('btn-run-publish').onclick = async () => {
    const useDemo = $('pub-use-demo').checked;
    const folder = $('pub-folder').value.trim();
    if (!useDemo && !folder) return toast('Escolha a pasta do site ou marque "Usar Site Demo Interativo".');
    let pws = [];
    if ($('pub-access').value === 'encrypted') {
      pws = [...document.querySelectorAll('#pub-pw-list .pub-pw')].map((i) => i.value).filter(Boolean);
      if (!pws.length) return toast('Digite pelo menos 1 senha.');
    }

    $('btn-run-publish').disabled = true;
    $('pub-progress').classList.remove('hidden');
    $('pub-result').classList.add('hidden');
    try {
      const job = await api('/api/publish', {
        method: 'POST',
        body: JSON.stringify({
          folder_path: folder,
          use_demo_site: useDemo,
          profile: $('pub-profile').value,
          art_theme: $('pub-theme').value,
          resolution: $('pub-res').value,
          fps: parseInt($('pub-fps').value, 10) || 15,
          shards: parseInt($('pub-shards')?.value || '1', 10) || 1,
          target_seconds: parseInt($('pub-seconds').value, 10) || 0,
          host_video_path: $('pub-host-video').value.trim(),
          audio_mode: $('pub-audio-mode')?.value || 'none',
          audio_source: $('pub-audio-source')?.value.trim() || '',
          music_style: $('pub-music-style')?.value || 'ambient',
          music_mood: $('pub-music-mood')?.value || 'dreamy',
          hide_header_bar: true,
          passwords: pws,
        }),
      });
      let pubPollActive = true;
      let pubErrs = 0;
      const pollPublish = async () => {
        if (!pubPollActive) return;
        try {
          const j = await api(`/api/jobs/${job.id}`);
          pubErrs = 0;
          $('pub-prog-detail').textContent = j.detail || 'Gerando...';
          $('pub-prog-pct').textContent = `${j.progress || 0}%`;
          $('pub-prog-bar').style.width = `${j.progress || 0}%`;
          if (j.status === 'done' && j.result) {
            pubPollActive = false;
            $('btn-run-publish').disabled = false;
            const outP = j.result.output_path;
            const openAddr = j.result.combined_address || outP;
            const shCnt = j.result.shard_count || 1;
            const shList = shCnt > 1 && Array.isArray(j.result.shard_paths)
              ? `<div class="muted" style="font-size:11.5px;margin:4px 0">🧩 <b>${shCnt} vídeos paralelos gerados</b> (subindo no YouTube, cole <code>btweb://ID1+ID2+...</code> na barra):<br>${j.result.shard_paths.map((p, idx) => `• Parte ${idx + 1}: <code>${esc(p)}</code>`).join('<br>')}</div>`
              : `<code style="display:inline-block;margin:6px 0">${esc(outP)}</code>`;
            const resEl = $('pub-result');
            resEl.classList.remove('hidden');
            resEl.innerHTML = `
              <div class="hint-box" style="border-color:var(--ok)">
                <b>✅ ${shCnt > 1 ? `Site dividido em ${shCnt} vídeos paralelos com sucesso!` : 'Vídeo do site gerado com sucesso!'}</b> (${j.result.total_frames} frames · ${fmtBytes(j.result.video_size_bytes)})<br>
                ${shList}
                <div style="display:flex;gap:8px;margin-top:6px;flex-wrap:wrap">
                  <button type="button" class="btn primary sm" id="btn-pub-open-now">🌐 Abrir Agora${shCnt > 1 ? ` (${shCnt} vídeos em paralelo)` : ' no BrightWeb'}</button>
                  <button type="button" class="btn sm" id="btn-pub-show-folder">📂 Mostrar .mp4 no Explorer</button>
                </div>
              </div>`;
            $('btn-pub-open-now').onclick = () => {
              closePublishModal();
              const tab = getActiveTab() || createTab();
              navigateTab(tab, openAddr, { passwords: pws });
            };
            $('btn-pub-show-folder').onclick = () => api('/api/system/open-folder', { method: 'POST', body: JSON.stringify({ path: outP }) });
            return;
          } else if (j.status === 'error') {
            pubPollActive = false;
            $('btn-run-publish').disabled = false;
            $('pub-result').classList.remove('hidden');
            $('pub-result').innerHTML = `<div class="err-box">❌ ${esc(j.error)}</div>`;
            return;
          }
        } catch (err) {
          pubErrs++;
          if (pubErrs >= 10) {
            pubPollActive = false;
            $('btn-run-publish').disabled = false;
            $('pub-result').classList.remove('hidden');
            $('pub-result').innerHTML = `<div class="err-box">❌ Perda de conexão com o processo local: ${esc(err.message)}</div>`;
            return;
          }
        }
        if (pubPollActive) setTimeout(pollPublish, 550);
      };
      setTimeout(pollPublish, 400);
    } catch (err) {
      $('btn-run-publish').disabled = false;
      toast('❌ ' + err.message, 5000);
    }
  };

  // Keyboard shortcuts
  document.addEventListener('keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 't') { e.preventDefault(); createTab(); }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'l') { e.preventDefault(); $('address-input').focus(); $('address-input').select(); }
    if (e.key === 'Escape' && !$('modal-publish').classList.contains('hidden')) closePublishModal();
  });

  // Heartbeat & clean shutdown
  setInterval(() => { fetch('/api/heartbeat', { method: 'POST', credentials: 'same-origin' }).catch(() => { }); }, 8000);
  fetch('/api/heartbeat', { method: 'POST', credentials: 'same-origin' }).catch(() => { });

  // Boot
  (async () => {
    try {
      const st = await api('/api/status');
      state.isolated = !!st.isolated_mode;
      $('btn-isolated').classList.toggle('active', state.isolated);
      if (st.prefs) {
        if (Array.isArray(st.prefs.bookmarks)) state.bookmarks = st.prefs.bookmarks;
        if (Array.isArray(st.prefs.history)) state.history = st.prefs.history;
        if (st.prefs.theme && window.BV_THEME) window.BV_THEME.apply(st.prefs.theme);
        if (st.prefs.language && window.BV_I18N) window.BV_I18N.apply(st.prefs.language);
      }
      if (st.update) renderUpdatePill(st.update);
      // Poll update status again after 4s to catch startup GitHub check result
      setTimeout(() => { api('/api/update/status').then(renderUpdatePill).catch(() => { }); }, 4000);
      if (st.art_themes && $('pub-theme')) {
        $('pub-theme').innerHTML = st.art_themes.map((t) => `<option value="${esc(t.code)}">${esc(t.name)}</option>`).join('');
        if ($('pub-theme').querySelector('option[value="julia-turing"]')) $('pub-theme').value = 'julia-turing';
      }
      const t = st.tools || {};
      $('home-tools-status').textContent = `FFmpeg: ${t.has_ffmpeg ? '✅ OK' : '❌ Ausente'} · yt-dlp: ${t.has_yt_dlp ? '✅ OK' : '❌ Ausente'}`;
      renderHomeLists();
      createTab(st.initial_target || '');
    } catch (err) {
      createTab();
      toast('❌ Erro inicializando BrightWeb: ' + err.message, 6000);
    }
  })();
})();
