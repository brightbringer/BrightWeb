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
    dnsBook: { name: 'Minha Lista', version: 1, entries: [] },
    dnsCollapsed: {},
    dnsFavOnly: false,
    dnsSearch: '',
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
      streamPollTimer: null,
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
    clearInterval(state.tabs[idx].streamPollTimer);
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
    const isBM = !!(tab.address && (
      state.bookmarks.some((b) => b.address === tab.address) ||
      (state.dnsBook?.entries || []).some((e) => e.kind === 'site' && (e.source === tab.address || (e.alias && `btweb://${e.alias}` === tab.address) || (tab.site?.source && e.source === tab.site.source)))
    ));
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
    const dnsFavs = (state.dnsBook?.entries || [])
      .filter((e) => e.kind === 'site' && e.favorite && e.source)
      .map((e) => ({
        title: e.name,
        address: e.alias ? `btweb://${e.alias}` : e.source,
        dnsId: e.id,
        hasPw: (e.passwords || []).length > 0,
      }));
    const seenAddr = new Set(dnsFavs.map((x) => x.address));
    const combinedBM = [
      ...dnsFavs,
      ...state.bookmarks.filter((b) => !seenAddr.has(b.address)),
    ];
    bmEl.innerHTML = combinedBM.length
      ? combinedBM.map((b) => `
        <div class="link-item" data-open-addr="${esc(b.address)}" ${b.dnsId ? `data-open-dns-id="${esc(b.dnsId)}"` : ''}>
          <div style="min-width:0"><div class="link-item-title">⭐ ${b.hasPw ? '🔒 ' : ''}${esc(b.title)}</div><div class="link-item-url">${esc(b.address)}</div></div>
        </div>`).join('')
      : '<p class="muted" style="font-size:12px">Nenhum favorito salvo ainda. Abra 📒 Minha Lista ou clique em ☆ na barra de endereço ao visitar um site.</p>';

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
        <div class="insp-stat"><span>Frames Lidos</span><b>${s.decoded_frames ? `${s.decoded_frames}/${s.total_frames}` : s.total_frames}</b></div>
        <div class="insp-stat"><span>Erros Corrigidos (FEC)</span><b>${s.corrected_bytes} B</b></div>
        <div class="insp-stat"><span>Tamanho Site (RAM)</span><b>${fmtBytes(s.total_bytes)}</b></div>
        <div class="insp-stat"><span>Criptografia</span><b>${esc(encLbl)}</b></div>
        ${s.stream_mode ? `<div class="insp-stat"><span>Streaming</span><b>${s.streaming ? `📡 Ao vivo (${s.loaded_files || 0}/${s.total_files || 0})` : (s.stream_mode === 'cas-instant' ? '⚡ Cache CAS (0 ms)' : s.stream_mode === 'online-live' ? '✅ Online Direto' : '✅ Progressivo')}</b></div>` : ''}
        ${s.cas_cache_hits > 0 ? `<div class="insp-stat"><span>Cache CAS (SHA-256)</span><b>⚡ ${s.cas_cache_hits}/${s.total_files || s.loaded_files} arq.</b></div>` : ''}
      </div>
      <div><b>Isolamento:</b> ${state.isolated ? '<span style="color:var(--ok)">🛡️ CSP Offline Ativo</span>' : '<span style="color:#fbbf24">⚠️ Conexões externas permitidas</span>'}</div>
      <div><b>Arquivos no Vídeo (${(s.files || []).length}${s.total_files && s.total_files > (s.files || []).length ? ` / ${s.total_files}` : ''}):</b></div>
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
      renderStreamPill(tab.site);
    } else if (tab.view === 'locked') {
      renderStreamPill(null);
      $('lock-desc').textContent = `Este site óptico está protegido por ${tab.lockLayers} camada(s) independente(s) de criptografia Pós-Quântica (Argon2id + AES-256-GCM + XChaCha20-Poly1305).`;
      const list = $('lock-pw-list');
      list.innerHTML = Array.from({ length: tab.lockLayers }, (_, i) =>
        `<input type="password" class="lock-pw-in" placeholder="Senha ${i + 1} de ${tab.lockLayers}">`
      ).join('');
      $('lock-error').classList.add('hidden');
      setTimeout(() => list.querySelector('input')?.focus(), 50);
    } else if (tab.view === 'error') {
      renderStreamPill(null);
      $('err-detail').textContent = tab.errorMsg || 'Erro desconhecido';
    } else {
      renderStreamPill(null);
    }
  }

  function renderStreamPill(site) {
    const pill = $('stream-pill');
    if (!pill) return;
    if (!site || !site.streaming) {
      pill.classList.add('hidden');
      return;
    }
    const modeLbl = site.stream_mode === 'online-live' ? '📡 Assistindo online' : '⚡ Carregando camadas';
    const loaded = site.loaded_files || (site.files || []).length;
    const total = site.total_files || loaded;
    const pct = site.total_frames > 0 && site.decoded_frames > 0
      ? Math.min(99, Math.round((site.decoded_frames * 100) / site.total_frames))
      : (total > 0 ? Math.min(99, Math.round((loaded * 100) / total)) : 0);
    pill.textContent = `${modeLbl} · ${loaded}/${total} arquivo(s) (${pct}%)`;
    pill.classList.remove('hidden');
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
    clearInterval(tab.streamPollTimer);
    tab.address = addr;
    if (opts.passwords && opts.passwords.length) tab.lastPasswords = [...opts.passwords];
    else if (!opts.keepPasswords) tab.lastPasswords = [];
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
    clearInterval(tab.streamPollTimer);
    tab.site = site;
    tab.view = 'site';
    tab.title = site.title || site.display_address || 'Site Óptico';
    tab.address = site.display_address || tab.address;
    // Force iframe reload cleanly
    $('site-frame').dataset.loadedUrl = '';
    recordGlobalHistory(tab.title, tab.address);
    renderAll();
    if (site.streaming) {
      const sid = site.site_id;
      tab.streamPollTimer = setInterval(async () => {
        try {
          const fresh = await api(`/api/sites/${sid}`);
          if (!tab.site || tab.site.site_id !== sid) {
            clearInterval(tab.streamPollTimer);
            return;
          }
          tab.site = fresh;
          if (tab.id === state.activeTabId) {
            renderStreamPill(fresh);
            renderInspector();
          }
          if (!fresh.streaming) {
            clearInterval(tab.streamPollTimer);
            if (fresh.stream_error) {
              toast(`⚠️ Stream parcial: ${fresh.stream_error}`, 5000);
            } else {
              toast(`✅ Site 100% carregado (${fresh.loaded_files || (fresh.files || []).length} arquivo(s))`, 2500);
            }
          }
        } catch {
          clearInterval(tab.streamPollTimer);
        }
      }, 500);
    }
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
    clearInterval(tab.streamPollTimer);
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
    if (!tab || !tab.address) {
      $('dns-sidebar')?.classList.remove('hidden');
      $('btn-dns-sidebar')?.classList.add('active');
      return toast('Abra um site primeiro ou adicione manualmente em 📒 Minha Lista.');
    }
    const curSrc = tab.site?.source || tab.address;
    const existing = (state.dnsBook?.entries || []).find((e) =>
      e.kind === 'site' && (e.source === curSrc || (e.alias && `btweb://${e.alias}` === tab.address))
    );
    if (existing) {
      openDNSEditModal(existing);
    } else {
      openDNSEditModal({
        kind: 'site',
        name: (tab.site?.title || tab.title || tab.address).replace(/^BrightWeb\s*·\s*/i, ''),
        source: curSrc,
        passwords: tab.lastPasswords || [],
        favorite: true,
        parent_id: '',
      });
    }
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

  // GitHub Auto-Update Badge (BrightWeb v0.701)
  state.updateInfo = null;
  const renderUpdatePill = (u) => {
    state.updateInfo = u;
    const btn = $('bw-update-btn');
    if (!btn || !u) return;
    const cur = u.current_version || '0.701';
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

  // =========================================================================
  // PUBLISH WEBSITE STUDIO (BrightVault-parity bars, GIF preview, audio studio & web estimates!)
  // =========================================================================
  state.statusMeta = null;
  state.pubSiteStats = null;
  let pubMusicSeed = Math.floor(Math.random() * 900000000) + 100000;

  const isStaticImagePath = (p) => {
    const clean = String(p || '').trim().replace(/^["']|["']$/g, '').split(/[?#]/)[0].toLowerCase();
    return /\.(jpe?g|png|webp|bmp|tiff?|avif)$/i.test(clean);
  };

  const resolveCodecTag = (prof, vcodec) => {
    const c = (vcodec || 'auto').toLowerCase();
    if (c === 'h264' || c === 'h264-mp4') return 'H.264 .mp4';
    if (c === 'av1' || c === 'av1-mp4') return 'AV1 .mp4';
    if (c === 'av1-mkv') return 'AV1 .mkv';
    if (c === 'vp9' || c === 'vp9-webm') return 'VP9 .webm';
    if (c === 'hevc' || c === 'hevc-mp4') return 'HEVC .mp4';
    if (prof === 'y8-gray' || prof === 'y6-gray' || prof === 'y4-gray' || prof === 'y2-safe' ||
        prof === 'art-chroma' || prof === 'art-crystal' || prof === 'art-fractal3d' || prof === 'art-angles') return 'AV1 .mp4';
    return 'H.264 .mp4';
  };

  const getValidFPSOptions = (res, prof) => {
    const all = [
      { v: 10, l: '10 FPS (Extra-Seguro)', rec: false },
      { v: 15, l: '15 FPS (Recomendado)', rec: true },
      { v: 24, l: '24 FPS (Fluido)', rec: false },
      { v: 30, l: '30 FPS (2× Capacidade)', rec: false },
    ];
    if (prof === 'y4-gray' || prof === 'y2-safe') {
      all.push({ v: 60, l: '60 FPS (Máx. Capacidade)', rec: false });
    }
    if (res === '4k' && prof.startsWith('stealth-')) {
      return all.filter((o) => o.v <= 24);
    }
    return all;
  };

  const computePubMetrics = (res, prof, fps, vcodec, maskIntensity, hideHeader, crossFEC, hostSource, shards) => {
    fps = Number(fps) || 15;
    shards = Math.max(1, Number(shards) || 1);
    let size = 7, up = 7, dl = 7, safe = 8, art = 1, estUp = 5.3, estDl = 6.5;
    const tag = resolveCodecTag(prof, vcodec);
    const useAV1 = tag.startsWith('AV1');
    const useVP9 = tag.startsWith('VP9');
    const effHost = String(hostSource || '').trim();
    const hasHost = effHost !== '';
    const isStaticImg = isStaticImagePath(effHost);
    const clamp10 = (v) => Math.max(1, Math.min(10, Math.round(v * 10) / 10));
    const clampInt = (v) => Math.max(1, Math.min(10, Math.round(v)));
    const fpsScale = (baseFPS, f) => Math.pow(baseFPS / Math.max(1, fps), f);

    const fpsSafePen = Math.max(0, Math.log2(fps / 8)) * 1.15;
    const resSafePen = res === '4k' ? 1.2 : res === '2k' ? 0.7 : res === '1080p' ? 0.35 : 0.0;
    const extremeLowResCapPen = (res === '720p' && fps >= 48) ? 1.8 : 0.0;

    const geoArtProfs = new Set(['art-fullres', 'art-crystal', 'art-fractal3d', 'art-angles', 'art-fractal-angles', 'art-bauhaus', 'art-constel', 'art-truchet']);
    const stealthProfs = new Set(['stealth-vhs', 'stealth-rain', 'stealth-film', 'stealth-stars']);
    const canHideHdr = (prof === 'art-stego' || prof === 'video-fractal' || prof === 'video-chroma' || prof === 'video-bw' || geoArtProfs.has(prof) || stealthProfs.has(prof));
    const effHideHdr = canHideHdr && !!hideHeader;
    const hdrSafeDelta = canHideHdr ? (effHideHdr ? -0.4 : 0.3) : 0;
    const hdrArtDelta = canHideHdr ? (effHideHdr ? 0.4 : -1.6) : 0;
    const hdrAreaFactor = effHideHdr ? (res === '720p' ? 1.05 : res === '1080p' ? 1.03 : res === '2k' ? 1.015 : 1.0) : 1.0;

    if (prof === 'y8-gray') {
      art = 1;
      estUp = +((res === '4k' ? 5.3 : res === '2k' ? 5.2 : 5.4) * fpsScale(10, 0.08)).toFixed(1);
      estDl = +((res === '4k' ? 5.8 : res === '2k' ? 6.5 : 7.4) * fpsScale(10, 0.25)).toFixed(1);
      size = clampInt(7.5 + (fps >= 15 ? 1 : 0) + (res === '2k' ? 0.5 : 0));
      up = clampInt(7.5 + (fps >= 15 ? 1.5 : 0.5));
      dl = clampInt(7 + (res === '4k' ? 2 : res === '2k' ? 1 : 0) + (fps >= 15 ? 1 : 0));
      safe = clamp10(9.2 - fpsSafePen - resSafePen);
      if (useAV1) { size = clampInt(size + 1.5); up = 10; dl = 10; safe = clamp10(safe + 0.6); estUp = +(estUp * 0.67).toFixed(1); estDl = +(estDl * 0.75).toFixed(1); }
      else if (useVP9) { size = clampInt(size + 1); up = 8; dl = 9; safe = clamp10(safe + 0.4); estUp = +(estUp * 0.80).toFixed(1); estDl = +(estDl * 0.85).toFixed(1); }
    } else if (prof === 'y6-gray') {
      art = 1;
      estUp = +((res === '720p' ? 4.9 : res === '1080p' ? 5.0 : 5.2) * fpsScale(10, 0.10)).toFixed(1);
      estDl = +((res === '4k' ? 7.2 : res === '2k' ? 7.6 : 8.4) * fpsScale(10, 0.28)).toFixed(1);
      size = clampInt(7 + (fps >= 20 ? 2 : fps >= 15 ? 1 : 0));
      up = clampInt(7 + (fps >= 20 ? 2 : fps >= 15 ? 1.5 : 1));
      dl = clampInt(7 + (fps >= 20 ? 2.5 : fps >= 15 ? 1.5 : 0.5));
      safe = clamp10(9.8 - fpsSafePen - resSafePen);
      if (useAV1) { size = clampInt(size + 1.5); up = 10; dl = 10; safe = clamp10(safe + 0.5); estUp = +(estUp * 0.69).toFixed(1); estDl = +(estDl * 0.72).toFixed(1); }
      else if (useVP9) { size = clampInt(size + 1); up = 8; dl = 9; safe = clamp10(safe + 0.4); estUp = +(estUp * 0.84).toFixed(1); estDl = +(estDl * 0.78).toFixed(1); }
    } else if (prof === 'y4-gray') {
      art = 1;
      estUp = +((res === '720p' ? 4.8 : res === '1080p' ? 5.0 : 5.3) * fpsScale(15, 0.15)).toFixed(1);
      estDl = +((res === '720p' ? 8.8 : res === '1080p' ? 9.2 : 9.8) * fpsScale(15, 0.25)).toFixed(1);
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
    } else {
      const mi = maskIntensity || 'balanced';
      const miSizeBoost = mi === 'max' ? 3.0 : mi === 'strong' ? 2.0 : mi === 'subtle' ? -1.5 : 0;
      const miSafeDelta = mi === 'max' ? -3.4 : mi === 'strong' ? -1.6 : mi === 'subtle' ? 1.1 : 0;
      const miArtDelta  = mi === 'max' ? -2.8 : mi === 'strong' ? -1.2 : mi === 'subtle' ? 0.8 : 0;
      const miEstFactor = (mi === 'max' ? 0.36 : mi === 'strong' ? 0.58 : mi === 'subtle' ? 3.1 : 1.0) * hdrAreaFactor;

      if (stealthProfs.has(prof)) {
        const stSpec = {
          'stealth-vhs':   { upH: 75, dl: 65, safe: 9.8, art: 9.8 },
          'stealth-stars': { upH: 78, dl: 64, safe: 9.9, art: 10.0 },
          'stealth-rain':  { upH: 92, dl: 78, safe: 9.5, art: 10.0 },
          'stealth-film':  { upH: 85, dl: 70, safe: 7.4, art: 10.0 },
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
          'art-fullres':        { upH: 7.4,  upA: 9.6,  dl: 8.1,  safe: 9.2, art: 9.7 },
          'art-crystal':        { upH: 7.0,  upA: 6.9,  dl: 7.1,  safe: 9.2, art: 8.6 },
          'art-fractal3d':      { upH: 7.8,  upA: 7.6,  dl: 7.3,  safe: 9.4, art: 8.9 },
          'art-constel':        { upH: 9.2,  upA: 23.5, dl: 18.6, safe: 9.5, art: 8.8 },
          'art-bauhaus':        { upH: 10.1, upA: 34.8, dl: 17.4, safe: 9.1, art: 9.0 },
          'art-angles':         { upH: 11.4, upA: 11.1, dl: 9.4,  safe: 8.8, art: 9.1 },
          'art-fractal-angles': { upH: 13.0, upA: 29.1, dl: 12.6, safe: 8.7, art: 9.5 },
          'art-truchet':        { upH: 18.3, upA: 31.2, dl: 19.4, safe: 8.6, art: 9.6 },
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
      } else if (prof === 'art-stego') {
        estUp = +((res === '4k' ? 12.7 : res === '2k' ? 14.1 : res === '1080p' ? 13.8 : 16.2) * fpsScale(10, 0.16) * miEstFactor).toFixed(1);
        estDl = +((res === '4k' ? 16.0 : res === '2k' ? 22.0 : res === '1080p' ? 18.0 : 17.5) * fpsScale(10, 0.24) * miEstFactor).toFixed(1);
        size = clampInt((res === '4k' ? 3 : 2) + (fps >= 20 ? 2 : fps >= 15 ? 1 : 0) + miSizeBoost);
        up = clampInt((res === '4k' ? 4 : 3) + (fps >= 20 ? 2 : fps >= 15 ? 1 : 0) + miSizeBoost * 0.8);
        dl = clampInt((res === '4k' ? 5 : 4) + (fps >= 20 ? 2 : fps >= 15 ? 1 : 0) + miSizeBoost * 0.8);
        safe = clamp10(9.8 - fpsSafePen - resSafePen + miSafeDelta + hdrSafeDelta);
        art = clamp10(9.4 + miArtDelta + hdrArtDelta);
      } else if (prof === 'art-chroma') {
        const acFactor = mi === 'max' ? 0.72 : mi === 'strong' ? 0.86 : mi === 'subtle' ? 3.0 : 1.0;
        estUp = +((useAV1 ? 5.0 : (res === '720p' ? 7.5 : 6.1)) * fpsScale(15, 0.14) * acFactor).toFixed(1);
        estDl = +((useAV1 ? 6.3 : (res === '720p' ? 10.5 : 9.5)) * fpsScale(15, 0.22) * acFactor).toFixed(1);
        size = clampInt((useAV1 ? 8 : 6) + (fps >= 20 ? 1 : 0) + miSizeBoost * 0.5);
        up = clampInt((useAV1 ? 8 : 6) + (fps >= 20 ? 1 : 0) + miSizeBoost * 0.4);
        dl = clampInt((useAV1 ? 8 : 6) + (fps >= 20 ? 1 : 0) + miSizeBoost * 0.4);
        safe = clamp10((useAV1 ? 9.6 : 9.3) - fpsSafePen - resSafePen + miSafeDelta * 0.9);
        art = clamp10(7.4 + miArtDelta * 0.8);
      } else if (prof === 'video-fractal') {
        estUp = +((res === '720p' ? 56.0 : res === '1080p' ? 44.0 : 40.0) * fpsScale(15, 0.22) * miEstFactor).toFixed(1);
        estDl = +((res === '720p' ? 36.0 : res === '1080p' ? 29.0 : 26.0) * fpsScale(15, 0.26) * miEstFactor).toFixed(1);
        size = clampInt((fps >= 30 ? 3.5 : fps >= 24 ? 2.5 : fps >= 15 ? 1.8 : 1) + miSizeBoost);
        up = clampInt((fps >= 30 ? 4 : fps >= 24 ? 3 : fps >= 15 ? 2 : 1) + miSizeBoost * 0.8);
        dl = clampInt((fps >= 30 ? 4.5 : fps >= 24 ? 3.5 : fps >= 15 ? 2.5 : 1.5) + miSizeBoost * 0.8);
        safe = clamp10(10.0 - fpsSafePen * 0.95 - resSafePen * 0.85 + miSafeDelta + hdrSafeDelta);
        art = clamp10(9.6 + miArtDelta + hdrArtDelta);
      } else {
        estUp = +((res === '720p' ? 53.0 : res === '1080p' ? 42.0 : 38.0) * fpsScale(15, 0.22) * miEstFactor).toFixed(1);
        estDl = +((res === '720p' ? 34.0 : res === '1080p' ? 28.0 : 25.0) * fpsScale(15, 0.26) * miEstFactor).toFixed(1);
        size = clampInt((fps >= 30 ? 4 : fps >= 24 ? 3 : fps >= 15 ? 2 : 1.2) + miSizeBoost);
        up = clampInt((fps >= 30 ? 4 : fps >= 24 ? 3 : fps >= 15 ? 2 : 1.2) + miSizeBoost * 0.8);
        dl = clampInt((fps >= 30 ? 4.5 : fps >= 24 ? 3.5 : fps >= 15 ? 2.5 : 1.5) + miSizeBoost * 0.8);
        safe = clamp10(10.0 - fpsSafePen * 0.9 - resSafePen * 0.8 + miSafeDelta * 0.9 + hdrSafeDelta);
        art = clamp10(8.0 + miArtDelta * 0.8 + hdrArtDelta);
      }
    }

    // Static Image Background effect (real YouTube VP9 VBR starvation on 0%-motion backgrounds!)
    let staticImgWarn = '';
    if (isStaticImg) {
      estUp = +(estUp * 0.84).toFixed(1);
      estDl = +(estDl * 0.88).toFixed(1);
      size = clampInt(size + 0.6);
      if (prof === 'stealth-film') {
        safe = clamp10(safe - 2.4);
        staticImgWarn = '🖼️ <b>Imagem estática + Cinema 35mm:</b> o YouTube derruba o bitrate em fundos parados e apaga o grão fino 16×16! Prefira <b>Estrelas (stealth-stars)</b>, <b>VHS (stealth-vhs)</b> ou um <b>vídeo com movimento</b>.';
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

    // Streaming + Decode 4x + Parallel yt-dlp + Shards speed boosts
    dl = clampInt(dl + 1.2 + (shards >= 4 ? 1.5 : shards >= 2 ? 0.8 : 0));
    if (useVP9 || useAV1) up = clampInt(up + 0.8);

    // Dynamic Cross-Frame RS Shield effect
    const isArtOrStealth = prof.startsWith('stealth-') || prof.startsWith('art-') || prof.startsWith('video-');
    const cfMode = (crossFEC || 'auto').toLowerCase();
    let cfActive = false, cfOverhead = 1.0, cfTag = '';
    if (cfMode === 'max') { cfActive = true; cfOverhead = 1.33; cfTag = '🛡️ Blindagem Máxima (K=6,M=2 · 25%)'; }
    else if (cfMode === 'high') { cfActive = true; cfOverhead = 1.25; cfTag = '🛡️ Blindagem Alta (K=8,M=2 · 20%)'; }
    else if (cfMode === 'auto' && isArtOrStealth) { cfActive = true; cfOverhead = 1.33; cfTag = '🛡️ Blindagem Auto (K=6,M=2 · 25%)'; }
    const safeMin = cfActive ? 5.0 : 6.0;
    const safeRec = cfActive ? 6.0 : 7.0;
    if (cfActive) {
      estUp = +(estUp * cfOverhead).toFixed(1);
      estDl = +(estDl * cfOverhead).toFixed(1);
      size = clampInt(size - (cfOverhead > 1.3 ? 0.8 : 0.5));
    }
    return { size, up, dl, safe, art, estUp, estDl, codecLabel: tag, fps, safeMin, safeRec, canHideHdr, effHideHdr, cfActive, cfTag, cfOverhead, isStaticImg, staticImgWarn, shards };
  };

  const getFramePayloadBytes = (res, prof, mi, hideHdr, hasHost) => {
    const det = state.statusMeta?.frame_capacities_detailed?.[res]?.[prof];
    if (det) {
      const miNum = mi === 'subtle' ? 1 : mi === 'strong' ? 2 : mi === 'max' ? 3 : 0;
      const hh = (hideHdr && prof !== 'art-chroma') ? 1 : 0;
      const k = `${miNum}_${hh}`;
      if (hasHost && det[k + '_host']) return det[k + '_host'];
      if (det[k]) return det[k];
    }
    return state.statusMeta?.frame_capacities?.[res]?.[prof] || 6000;
  };

  const fmtDuration = (sec) => {
    sec = Math.max(1, Math.round(sec));
    if (sec < 60) return `${sec}s`;
    const m = Math.floor(sec / 60), s = sec % 60;
    if (m < 60) return `${m}m ${String(s).padStart(2, '0')}s`;
    const h = Math.floor(m / 60), rm = m % 60;
    return `${h}h ${String(rm).padStart(2, '0')}m`;
  };

  const renderPubAttrBars = () => {
    const el = $('pub-attr-bars');
    if (!el) return;
    const res = $('pub-res')?.value || '1080p';
    const prof = $('pub-profile')?.value || 'art-fullres';
    const fps = Number($('pub-fps')?.value || 15);
    const vcodec = $('pub-vcodec')?.value || 'auto';
    const mi = $('pub-mask-intensity')?.value || 'balanced';
    const hideHdr = !!$('pub-hide-header')?.checked;
    const crossFEC = $('pub-cross-fec')?.value || 'auto';
    const hostSrc = $('pub-host-video')?.value || '';
    const shards = parseInt($('pub-shards')?.value || '1', 10) || 1;
    const m = computePubMetrics(res, prof, fps, vcodec, mi, hideHdr, crossFEC, hostSrc, shards);

    const barColor = (v, isSafe) => {
      if (isSafe) return v >= m.safeRec ? '#22c55e' : v >= m.safeMin ? '#f59e0b' : '#ef4444';
      return v >= 8 ? '#22c55e' : v >= 5 ? '#38bdf8' : v >= 3 ? '#f59e0b' : '#ef4444';
    };
    const fmtScore = (v) => Number.isInteger(v) ? String(v) : v.toFixed(1).replace('.', ',');
    const rows = [
      ['📦 Tamanho (Menor Vídeo)', m.size, false],
      ['⬆️ Velocidade Publicação', m.up, false],
      ['⬇️ Velocidade Abertura', m.dl, false],
      ['🛡️ Segurança (Anti-Erro)', m.safe, true],
      ['🎨 Nível Artístico', m.art, false],
    ];
    const safeBadge = m.safe >= m.safeRec
      ? `<span style="color:#4ade80;font-weight:600">✅ Recomendado (≥${m.safeRec.toFixed(1).replace('.', ',')}/10${m.cfActive ? ' com Blindagem Cross-Frame' : ''})</span>`
      : m.safe >= m.safeMin
        ? `<span style="color:#fbbf24;font-weight:600">⚠️ Zona mínima (${m.safeMin.toFixed(1).replace('.', ',')}–${m.safeRec.toFixed(1).replace('.', ',')}/10)${m.cfActive ? ' — protegido até 25% de apagão' : ''}</span>`
        : `<span style="color:#f87171;font-weight:700">❌ Abaixo do mínimo (${m.safeMin.toFixed(1).replace('.', ',')}/10) — risco real de falha no YouTube!</span>`;

    // Site-specific real estimate (duration, frames, MP4 size)
    let siteEstHtml = '';
    const siteBytes = $('pub-use-demo')?.checked ? 14500 : (state.pubSiteStats?.total_bytes || 0);
    if (siteBytes > 0) {
      const capPerFr = getFramePayloadBytes(res, prof, mi, hideHdr, hostSrc.trim() !== '');
      const compressedEst = Math.max(2048, siteBytes * 0.72); // conservative hybrid Zstd estimate
      const perShardBytes = compressedEst / shards;
      const dataFr = Math.max(1, Math.ceil(perShardBytes / Math.max(512, capPerFr)));
      const totFr = Math.max(2 + Math.ceil(dataFr * (dataFr >= 24 ? m.cfOverhead : 1)), (parseInt($('pub-seconds')?.value || '0', 10) || 0) * fps);
      const durStr = fmtDuration(totFr / fps);
      const estFileSize = Math.max(250 * 1024, siteBytes * 0.72 * m.estUp / shards);
      const shardNote = shards > 1 ? ` × ${shards} vídeos` : '';
      siteEstHtml = `<div style="margin-top:4px;color:var(--text)">🌐 <b>Estimativa p/ este site (${fmtBytes(siteBytes)}):</b> ~<b>${durStr}</b> (${totFr} frames${shardNote}) · ~<b>${fmtBytes(estFileSize)}</b>/vídeo</div>`;
    }
    const staticTag = m.isStaticImg ? ' · 🖼️ <b>Imagem Estática</b> (VP9 reduz bitrate)' : '';
    const cfHtml = m.cfTag ? ` · <b>${m.cfTag}</b>` : '';
    const staticWarnHtml = m.staticImgWarn ? `<div style="margin-top:5px;color:#fca5a5;font-weight:600">${m.staticImgWarn}</div>` : '';

    el.innerHTML = rows.map(([lbl, val, isSafe]) => `
      <div class="attr-row" style="${isSafe ? 'margin-top:8px' : ''}">
        <span class="attr-lbl">${lbl}</span>
        <div class="attr-track">
          <div class="attr-fill" style="width:${val * 10}%;background:${barColor(val, isSafe)}"></div>
          ${isSafe ? `<div class="attr-min-mark" style="left:${m.safeMin * 10}%" data-lbl="${m.safeMin} Mín"></div><div class="attr-rec-mark" style="left:${m.safeRec * 10}%" data-lbl="${m.safeRec} Rec"></div>` : ''}
        </div>
        <span class="attr-val" style="${isSafe ? (val < m.safeMin ? 'color:#f87171' : val < m.safeRec ? 'color:#fbbf24' : 'color:#4ade80') : ''}">${fmtScore(val)}/10</span>
      </div>`).join('') +
      `<div class="attr-summary">📊 <b>${m.codecLabel}</b> · <b>${res.toUpperCase()}</b> · <b>${m.fps} FPS</b>${cfHtml}${staticTag}<br>${safeBadge}${siteEstHtml}${staticWarnHtml}</div>`;
  };

  let pubPreviewTimer = null;
  const updatePubPreviewGIF = () => {
    const img = $('pub-style-gif');
    if (!img) return;
    clearTimeout(pubPreviewTimer);
    pubPreviewTimer = setTimeout(() => {
      const prof = $('pub-profile')?.value || 'art-fullres';
      const theme = $('pub-theme')?.value || 'julia-turing';
      const intensity = $('pub-mask-intensity')?.value || 'balanced';
      const hideHdr = !!$('pub-hide-header')?.checked;
      const hasHost = ($('pub-host-video')?.value.trim() || '') !== '';
      const isStealth = prof.startsWith('stealth-');
      const useHost = hasHost || prof === 'video-fractal' || prof === 'video-chroma' || (isStealth && theme === 'none');
      const qs = new URLSearchParams({
        profile: prof,
        theme,
        intensity,
        hide_header: hideHdr ? '1' : '0',
        use_host: useHost ? '1' : '0',
      });
      img.src = `/api/preview/gif?${qs.toString()}`;
      const profTxt = $('pub-profile')?.selectedOptions?.[0]?.textContent?.split('(')[0]?.trim() || prof;
      const themeTxt = $('pub-theme')?.selectedOptions?.[0]?.textContent?.trim() || theme;
      if ($('pub-style-gif-title')) {
        $('pub-style-gif-title').textContent = prof.startsWith('y') ? profTxt : `${profTxt} — ${hasHost ? 'Mídia Base' : themeTxt}`;
      }
      if ($('pub-style-gif-caption')) {
        $('pub-style-gif-caption').textContent = isStaticImagePath($('pub-host-video')?.value || '')
          ? '🖼️ Fundo de imagem estática selecionado — acompanhe a barra de Segurança abaixo antes de subir ao YouTube!'
          : 'Prévia animada real do padrão óptico. Clique na imagem para ampliar em tela grande.';
      }
    }, 120);
  };

  const refreshPubUI = (preferredFPS = null) => {
    const res = $('pub-res')?.value || '1080p';
    const prof = $('pub-profile')?.value || 'art-fullres';
    const hasHost = ($('pub-host-video')?.value.trim() || '') !== '';
    const isRawData = prof.startsWith('y');
    const theme = $('pub-theme')?.value || 'julia-turing';

    if ($('pub-art-theme-wrap')) {
      $('pub-art-theme-wrap').style.opacity = isRawData ? '0.45' : '1';
      $('pub-theme').disabled = isRawData;
      $('pub-art-keyword').disabled = isRawData || theme === 'none';
    }
    if ($('pub-host-wrap')) {
      $('pub-host-wrap').style.opacity = isRawData ? '0.45' : '1';
    }
    if ($('pub-mask-intensity-wrap')) {
      $('pub-mask-intensity').disabled = isRawData;
      $('pub-mask-intensity-wrap').style.opacity = isRawData ? '0.45' : '1';
    }
    if ($('pub-hide-header-row')) {
      const canHide = !isRawData && prof !== 'art-chroma' && (prof !== 'video-bw' || hasHost);
      $('pub-hide-header').disabled = !canHide;
      $('pub-hide-header-row').style.opacity = canHide ? '1' : '0.45';
    }
    if ($('pub-opt-audio-host')) {
      const isStaticImg = isStaticImagePath($('pub-host-video')?.value || '');
      $('pub-opt-audio-host').disabled = !hasHost || isStaticImg;
      if ((!hasHost || isStaticImg) && $('pub-audio-mode')?.value === 'host') {
        $('pub-audio-mode').value = 'none';
      }
    }
    if ($('pub-opt-dur-video')) {
      const isStaticImg = isStaticImagePath($('pub-host-video')?.value || '');
      $('pub-opt-dur-video').disabled = !hasHost || isStaticImg;
      if ((!hasHost || isStaticImg) && $('pub-duration-source')?.value === 'video') {
        $('pub-duration-source').value = 'files';
      }
    }
    const aMode = $('pub-audio-mode')?.value || 'none';
    $('pub-music-wrap')?.classList.toggle('hidden', aMode !== 'ambient');
    $('pub-audio-file-wrap')?.classList.toggle('hidden', aMode !== 'file');

    const opts = getValidFPSOptions(res, prof);
    const prev = preferredFPS != null ? Number(preferredFPS) : Number($('pub-fps')?.value || 15);
    if ($('pub-fps')) {
      $('pub-fps').innerHTML = opts.map((o) => `<option value="${o.v}">${esc(o.l)}</option>`).join('');
      const exact = opts.find((o) => o.v === prev);
      const rec = opts.find((o) => o.rec) || opts[0];
      $('pub-fps').value = String(exact ? exact.v : rec.v);
    }

    if ($('pub-prof-desc') && state.statusMeta?.profiles) {
      const pObj = state.statusMeta.profiles.find((x) => x.code === prof);
      $('pub-prof-desc').textContent = pObj ? `ℹ️ ${pObj.description}` : '';
    }
    renderPubAttrBars();
    updatePubPreviewGIF();
    refreshPubCapacityMeter(false);
  };

  // Mandatory Duration & Capacity Meter (warns clearly in RED when video/audio duration is insufficient!)
  state.pubMediaDurCache = {};
  let pubCapProbeTimer = null;
  const renderPubCapacityUI = (durSec, title, hintMsg = '') => {
    const box = $('pub-capacity-box');
    if (!box) return;
    const durSrc = $('pub-duration-source')?.value || 'files';
    $('btn-pub-reprobe-cap')?.classList.toggle('hidden', durSrc === 'files');
    if (durSrc === 'files' && !hintMsg) {
      box.classList.add('hidden');
      box.classList.remove('cap-warn');
      return;
    }
    box.classList.remove('hidden');
    if (!durSec || durSec <= 0) {
      box.classList.remove('cap-warn');
      $('pub-cap-header').textContent = '📊 Capacidade pela Duração Mandatória';
      $('pub-cap-pct').textContent = '—';
      $('pub-cap-bar').style.width = '0%';
      $('pub-cap-detail').innerHTML = hintMsg || 'Informe um vídeo ou áudio acima para calcular a capacidade.';
      return;
    }
    const res = $('pub-res')?.value || '1080p';
    const prof = $('pub-profile')?.value || 'art-fullres';
    const fps = Number($('pub-fps')?.value || 15);
    const mi = $('pub-mask-intensity')?.value || 'balanced';
    const hideHdr = !!$('pub-hide-header')?.checked;
    const hasHost = ($('pub-host-video')?.value.trim() || '') !== '';
    const shards = parseInt($('pub-shards')?.value || '1', 10) || 1;
    const cfMode = ($('pub-cross-fec')?.value || 'auto').toLowerCase();
    const isArtOrStealth = prof.startsWith('stealth-') || prof.startsWith('art-') || prof.startsWith('video-');
    const cfOverhead = cfMode === 'max' ? 1.33 : cfMode === 'high' ? 1.25 : (cfMode === 'auto' && isArtOrStealth ? 1.33 : 1.0);

    const capPerFr = getFramePayloadBytes(res, prof, mi, hideHdr, hasHost);
    const framesAvail = Math.max(1, Math.floor(durSec * fps) - 2);
    const usableFrames = Math.max(1, Math.floor(framesAvail / cfOverhead));
    const totalCapBytes = usableFrames * capPerFr * shards;
    const siteBytes = $('pub-use-demo')?.checked ? 14500 : (state.pubSiteStats?.total_bytes || 0);
    const neededBytes = siteBytes > 0 ? Math.max(2048, siteBytes * 0.72) : 0;
    const pct = totalCapBytes > 0 ? (neededBytes / totalCapBytes) * 100 : 0;
    const over = neededBytes > totalCapBytes;

    box.classList.toggle('cap-warn', over);
    $('pub-cap-header').textContent = `📊 Capacidade (${fmtDuration(durSec)}${shards > 1 ? ` × ${shards} shards` : ''})`;
    $('pub-cap-pct').textContent = neededBytes > 0 ? `${Math.round(pct)}%` : fmtBytes(totalCapBytes);
    $('pub-cap-pct').style.color = over ? '#f87171' : pct > 85 ? '#fbbf24' : '#4ade80';
    $('pub-cap-bar').style.width = `${Math.min(100, Math.max(2, pct))}%`;
    $('pub-cap-bar').style.background = over ? '#ef4444' : pct > 85 ? '#f59e0b' : '#22c55e';
    const label = title ? ` (${esc(title)})` : '';
    if (over) {
      const minDur = Math.ceil((neededBytes / Math.max(512, capPerFr * shards)) * cfOverhead / fps) + 2;
      $('pub-cap-detail').innerHTML = `<span style="color:#fca5a5;font-weight:700">⚠️ Duração insuficiente${label}!</span> O site precisa de ~<b>${fmtBytes(neededBytes)}</b>, mas ${fmtDuration(durSec)} comportam apenas <b>${fmtBytes(totalCapBytes)}</b> (precisaria de ~<b>${fmtDuration(minDur)}</b>). O vídeo será estendido automaticamente OU aumente <b>Shards / Resolução / FPS</b>!`;
    } else if (neededBytes > 0) {
      $('pub-cap-detail').innerHTML = `✅ Cabe com folga${label}: site usa ~<b>${fmtBytes(neededBytes)}</b> de <b>${fmtBytes(totalCapBytes)}</b> disponíveis (${framesAvail} frames/vídeo).`;
    } else {
      $('pub-cap-detail').innerHTML = `📦 Comporta até <b>${fmtBytes(totalCapBytes)}</b> de dados do site em ${fmtDuration(durSec)}${label}.`;
    }
  };

  const refreshPubCapacityMeter = async (forceProbe = false) => {
    const durSrc = $('pub-duration-source')?.value || 'files';
    if (durSrc === 'files') {
      renderPubCapacityUI(0, '');
      return;
    }
    let src = '';
    if (durSrc === 'video') {
      src = $('pub-host-video')?.value.trim() || '';
      if (!src || isStaticImagePath(src)) {
        renderPubCapacityUI(0, '', 'Escolha um <b>Vídeo Base em movimento</b> acima (imagens estáticas não têm duração) ou mude para "Arquivos do Site".');
        return;
      }
    } else if (durSrc === 'audio') {
      const aMode = $('pub-audio-mode')?.value || 'none';
      if (aMode === 'file') {
        src = $('pub-audio-source')?.value.trim() || '';
      } else if (aMode === 'host') {
        src = $('pub-host-video')?.value.trim() || '';
      }
      if (!src) {
        renderPubCapacityUI(0, '', 'Escolha um <b>Arquivo de Áudio Local</b> acima (em Trilha de Áudio → Arquivo de Áudio Local) para calcular a capacidade pela duração da música.');
        return;
      }
    }
    if (!forceProbe && state.pubMediaDurCache[src]) {
      const c = state.pubMediaDurCache[src];
      renderPubCapacityUI(c.duration_sec, c.title);
      return;
    }
    renderPubCapacityUI(0, '', '⏳ Consultando duração da mídia...');
    try {
      const info = await api('/api/media/probe-duration', { method: 'POST', body: JSON.stringify({ source: src }) });
      state.pubMediaDurCache[src] = info;
      renderPubCapacityUI(info.duration_sec, info.title);
    } catch (err) {
      renderPubCapacityUI(0, '', `⚠️ ${esc(err.message)}`);
    }
  };
  const schedulePubCapProbe = () => {
    clearTimeout(pubCapProbeTimer);
    pubCapProbeTimer = setTimeout(() => refreshPubCapacityMeter(true), 450);
  };

  let pubStatsTimer = null;
  const probePubFolderStats = () => {
    clearTimeout(pubStatsTimer);
    const infoEl = $('pub-site-info');
    if ($('pub-use-demo')?.checked) {
      state.pubSiteStats = { file_count: 4, total_bytes: 14500, has_index_html: true };
      if (infoEl) {
        infoEl.textContent = '✅ Site Demo Interativo selecionado (4 arquivos · ~14,5 KB · index.html pronto)';
        infoEl.classList.remove('hidden');
      }
      renderPubAttrBars();
      refreshPubCapacityMeter(false);
      return;
    }
    const p = $('pub-folder')?.value.trim() || '';
    if (!p) {
      state.pubSiteStats = null;
      infoEl?.classList.add('hidden');
      renderPubAttrBars();
      refreshPubCapacityMeter(false);
      return;
    }
    pubStatsTimer = setTimeout(async () => {
      try {
        const st = await api('/api/system/path-stats', { method: 'POST', body: JSON.stringify({ path: p }) });
        state.pubSiteStats = st;
        if (infoEl) {
          const idxTag = st.has_index_html ? '✅ index.html detectado' : '⚠️ sem index.html (abrirá índice automático)';
          infoEl.textContent = `📁 ${st.file_count} arquivo(s) · ${fmtBytes(st.total_bytes)} · ${idxTag}`;
          infoEl.classList.remove('hidden');
        }
        renderPubAttrBars();
        refreshPubCapacityMeter(false);
      } catch (err) {
        state.pubSiteStats = null;
        if (infoEl) {
          infoEl.textContent = `⚠️ ${err.message}`;
          infoEl.classList.remove('hidden');
        }
      }
    }, 250);
  };

  // Publish Website Modal — Audio Controls & Generative Preview
  const getPubMusicInstruments = () => [...document.querySelectorAll('#pub-music-insts input:checked')].map((el) => el.value);
  const playPubMusicPreview = () => {
    const player = $('pub-music-player');
    if (!player) return;
    const qs = new URLSearchParams({
      style: $('pub-music-style')?.value || 'ambient',
      mood: $('pub-music-mood')?.value || 'dreamy',
      instruments: getPubMusicInstruments().join(','),
      prompt: $('pub-music-prompt')?.value.trim() || '',
      seed: String(pubMusicSeed),
    });
    player.src = `/api/preview/audio?${qs.toString()}`;
    player.classList.remove('hidden');
    player.play().catch(() => { });
  };
  if ($('btn-pub-preview-music')) $('btn-pub-preview-music').onclick = playPubMusicPreview;
  if ($('btn-pub-reroll-music')) {
    $('btn-pub-reroll-music').onclick = () => {
      pubMusicSeed = Math.floor(Math.random() * 900000000) + 100000;
      playPubMusicPreview();
    };
  }
  if ($('pub-audio-mode')) $('pub-audio-mode').onchange = () => refreshPubUI();
  if ($('btn-pub-pick-audio')) {
    $('btn-pub-pick-audio').onclick = async () => {
      try {
        const res = await api('/api/system/pick-audio', { method: 'POST' });
        if (!res.cancelled && res.path) $('pub-audio-source').value = res.path;
      } catch (err) { toast('❌ ' + err.message); }
    };
  }

  // Expand preview zoom modal
  const openPubPreviewZoom = () => {
    const prof = $('pub-profile')?.value || 'art-fullres';
    const theme = $('pub-theme')?.value || 'julia-turing';
    const intensity = $('pub-mask-intensity')?.value || 'balanced';
    const hideHdr = !!$('pub-hide-header')?.checked;
    const hasHost = ($('pub-host-video')?.value.trim() || '') !== '';
    const isStealth = prof.startsWith('stealth-');
    const useHost = hasHost || prof === 'video-fractal' || prof === 'video-chroma' || (isStealth && theme === 'none');
    const qs = new URLSearchParams({
      profile: prof, theme, intensity,
      hide_header: hideHdr ? '1' : '0',
      use_host: useHost ? '1' : '0',
      size: 'large',
    });
    $('pub-zoom-img').src = `/api/preview/gif?${qs.toString()}`;
    $('pub-zoom-title').textContent = '🔍 ' + ($('pub-style-gif-title')?.textContent || 'Prévia Óptica');
    $('pub-zoom-cap').textContent = $('pub-prof-desc')?.textContent || '';
    $('modal-pub-preview-zoom').classList.remove('hidden');
  };
  if ($('btn-pub-expand-preview')) $('btn-pub-expand-preview').onclick = openPubPreviewZoom;
  if ($('pub-style-gif-wrap')) $('pub-style-gif-wrap').onclick = openPubPreviewZoom;
  if ($('btn-close-pub-zoom')) $('btn-close-pub-zoom').onclick = () => $('modal-pub-preview-zoom').classList.add('hidden');

  // Quick Web Presets
  document.querySelectorAll('[data-pub-preset]').forEach((btn) => {
    btn.onclick = () => {
      const k = btn.dataset.pubPreset;
      if (k === 'web-balanced') {
        $('pub-res').value = '1080p'; $('pub-profile').value = 'art-fullres'; $('pub-vcodec').value = 'auto';
        $('pub-shards').value = '1'; $('pub-cross-fec').value = 'auto'; $('pub-mask-intensity').value = 'balanced';
        refreshPubUI(15);
      } else if (k === 'web-stealth') {
        $('pub-res').value = '2k'; $('pub-profile').value = 'stealth-stars'; $('pub-vcodec').value = 'auto';
        $('pub-shards').value = '1'; $('pub-cross-fec').value = 'max'; $('pub-mask-intensity').value = 'balanced';
        if ($('pub-theme')?.querySelector('option[value="none"]')) $('pub-theme').value = 'none';
        refreshPubUI(15);
      } else if (k === 'web-heavy') {
        $('pub-res').value = '2k'; $('pub-profile').value = 'art-fullres'; $('pub-vcodec').value = 'auto';
        $('pub-shards').value = '4'; $('pub-cross-fec').value = 'auto'; $('pub-mask-intensity').value = 'balanced';
        refreshPubUI(15);
      } else if (k === 'web-fast') {
        $('pub-res').value = '720p'; $('pub-profile').value = 'y4-gray'; $('pub-vcodec').value = 'av1-mp4';
        $('pub-shards').value = '1'; $('pub-cross-fec').value = 'auto';
        refreshPubUI(30);
      }
    };
  });

  // Publish Website Modal open/close & field bindings
  const openPublishModal = () => {
    $('pub-progress').classList.add('hidden');
    $('pub-result').classList.add('hidden');
    $('modal-publish').classList.remove('hidden');
    refreshPubUI();
    probePubFolderStats();
  };
  const closePublishModal = () => {
    $('modal-publish').classList.add('hidden');
    const p = $('pub-music-player');
    if (p) p.pause();
  };
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
  if ($('btn-pub-toggle-pw')) {
    $('btn-pub-toggle-pw').onclick = () => {
      const inputs = [...document.querySelectorAll('#pub-pw-list .pub-pw')];
      const show = inputs.some((i) => i.type === 'password');
      inputs.forEach((i) => { i.type = show ? 'text' : 'password'; });
    };
  }
  $('btn-pub-pick-folder').onclick = async () => {
    try {
      const res = await api('/api/system/pick-folder', { method: 'POST' });
      if (!res.cancelled && res.path) {
        $('pub-folder').value = res.path;
        $('pub-use-demo').checked = false;
        probePubFolderStats();
      }
    } catch (err) { toast('❌ ' + err.message); }
  };
  $('pub-folder').oninput = probePubFolderStats;
  $('pub-use-demo').onchange = probePubFolderStats;

  const onPubHostChanged = () => {
    const val = $('pub-host-video').value.trim();
    if (val && $('pub-profile').value.startsWith('stealth-') && $('pub-theme').querySelector('option[value="none"]')) {
      $('pub-theme').value = 'none';
    }
    refreshPubUI();
  };
  $('btn-pub-pick-host').onclick = async () => {
    try {
      const res = await api('/api/system/pick-video', { method: 'POST' });
      if (!res.cancelled && res.path) {
        $('pub-host-video').value = res.path;
        onPubHostChanged();
      }
    } catch (err) { toast('❌ ' + err.message); }
  };
  if ($('btn-pub-clear-host')) {
    $('btn-pub-clear-host').onclick = () => { $('pub-host-video').value = ''; onPubHostChanged(); };
  }
  $('pub-host-video').oninput = onPubHostChanged;
  $('pub-profile').onchange = () => {
    if ($('pub-profile').value.startsWith('stealth-') && $('pub-host-video').value.trim() && $('pub-theme').querySelector('option[value="none"]')) {
      $('pub-theme').value = 'none';
    }
    refreshPubUI();
  };
  ['pub-res', 'pub-vcodec', 'pub-mask-intensity', 'pub-theme', 'pub-cross-fec', 'pub-shards'].forEach((id) => {
    if ($(id)) $(id).onchange = () => refreshPubUI();
  });
  if ($('pub-fps')) $('pub-fps').onchange = () => { renderPubAttrBars(); refreshPubCapacityMeter(false); };
  if ($('pub-seconds')) $('pub-seconds').oninput = () => renderPubAttrBars();
  if ($('pub-hide-header')) $('pub-hide-header').onchange = () => refreshPubUI();
  if ($('pub-duration-source')) {
    $('pub-duration-source').onchange = () => {
      const ds = $('pub-duration-source').value;
      if (ds === 'audio' && ($('pub-audio-mode').value === 'none' || $('pub-audio-mode').value === 'ambient')) {
        $('pub-audio-mode').value = 'file';
      }
      refreshPubUI();
      refreshPubCapacityMeter(true);
    };
  }
  if ($('btn-pub-reprobe-cap')) $('btn-pub-reprobe-cap').onclick = () => refreshPubCapacityMeter(true);
  if ($('pub-audio-source')) $('pub-audio-source').oninput = () => schedulePubCapProbe();
  if ($('btn-pub-pick-audio')) {
    $('btn-pub-pick-audio').onclick = async () => {
      try {
        const res = await api('/api/system/pick-audio', { method: 'POST' });
        if (!res.cancelled && res.path) {
          $('pub-audio-source').value = res.path;
          refreshPubCapacityMeter(true);
        }
      } catch (err) { toast('❌ ' + err.message); }
    };
  }

  // Background Publish Bottom Bar + Real Cancellation!
  let activePubJobId = null;
  let pubPollActive = false;

  const setPubBottomBar = (ico, title, pct, actionsHtml) => {
    const bar = $('pub-bottom-bar');
    if (!bar) return;
    bar.classList.remove('hidden');
    $('pub-bar-ico').textContent = ico;
    $('pub-bar-title').textContent = title;
    $('pub-bar-pct').textContent = `${Math.max(0, Math.min(100, pct || 0))}%`;
    $('pub-bar-fill').style.width = `${Math.max(0, Math.min(100, pct || 0))}%`;
    if (actionsHtml !== undefined) {
      $('pub-bar-actions').innerHTML = actionsHtml;
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
    if (pubPollActive && activePubJobId) {
      return toast('Já existe uma geração de vídeo em andamento na barra inferior. Cancele ou aguarde concluir.', 4500);
    }

    $('btn-run-publish').disabled = true;
    try {
      const job = await api('/api/publish', {
        method: 'POST',
        body: JSON.stringify({
          folder_path: folder,
          use_demo_site: useDemo,
          profile: $('pub-profile').value,
          video_codec: $('pub-vcodec')?.value || 'auto',
          mask_intensity: $('pub-mask-intensity')?.value || 'balanced',
          art_theme: $('pub-theme').value,
          art_keyword: $('pub-art-keyword')?.value.trim() || '',
          resolution: $('pub-res').value,
          fps: parseInt($('pub-fps').value, 10) || 15,
          shards: parseInt($('pub-shards')?.value || '1', 10) || 1,
          target_seconds: parseInt($('pub-seconds').value, 10) || 0,
          duration_source: $('pub-duration-source')?.value || 'files',
          padding_mode: $('pub-padding-mode')?.value || 'auto',
          optimize_png_lossless: !!$('pub-opt-png')?.checked,
          host_video_path: $('pub-host-video').value.trim(),
          audio_mode: $('pub-audio-mode')?.value || 'none',
          audio_source: $('pub-audio-source')?.value.trim() || '',
          music_style: $('pub-music-style')?.value || 'ambient',
          music_mood: $('pub-music-mood')?.value || 'dreamy',
          music_instruments: getPubMusicInstruments(),
          music_prompt: $('pub-music-prompt')?.value.trim() || '',
          cross_fec_mode: $('pub-cross-fec')?.value || 'auto',
          hide_header_bar: !!$('pub-hide-header')?.checked,
          passwords: pws,
        }),
      });
      // Immediately close publish modal & dock progress in bottom bar so the user keeps browsing freely!
      closePublishModal();
      $('btn-run-publish').disabled = false;
      activePubJobId = job.id;
      pubPollActive = true;
      setPubBottomBar('🚀', job.detail || 'Iniciando geração do vídeo do site...', 2,
        '<button type="button" class="btn sm danger" id="btn-pub-cancel-job">✕ Cancelar</button>');
      toast('🚀 Gerando vídeo em segundo plano — acompanhe pela barra inferior!', 3500);

      const bindCancelBtn = () => {
        const cb = $('btn-pub-cancel-job');
        if (!cb) return;
        cb.onclick = async () => {
          cb.disabled = true;
          cb.textContent = '⏳ Cancelando...';
          try {
            await api(`/api/jobs/${job.id}/cancel`, { method: 'POST' });
          } catch (e) { toast('⚠️ ' + e.message); }
        };
      };
      bindCancelBtn();

      let pubErrs = 0;
      const pollPublish = async () => {
        if (!pubPollActive || activePubJobId !== job.id) return;
        try {
          const j = await api(`/api/jobs/${job.id}`);
          pubErrs = 0;
          if (j.status === 'running') {
            setPubBottomBar('⚙️', j.detail || 'Gerando vídeo óptico...', j.progress || 0);
          } else if (j.status === 'done' && j.result) {
            pubPollActive = false;
            activePubJobId = null;
            const outP = j.result.output_path;
            const openAddr = j.result.combined_address || outP;
            const shCnt = j.result.shard_count || 1;
            const label = shCnt > 1
              ? `✅ ${shCnt} vídeos prontos! (${j.result.total_frames} frames · ${fmtBytes(j.result.video_size_bytes)})`
              : `✅ Vídeo pronto! (${j.result.total_frames} frames · ${fmtBytes(j.result.video_size_bytes)})`;
            setPubBottomBar('✅', label, 100, `
              <button type="button" class="btn primary sm" id="btn-bar-open-site">🌐 Abrir Site</button>
              <button type="button" class="btn sm" id="btn-bar-save-mysites">🏠 Salvar em Meus Sites</button>
              <button type="button" class="btn sm" id="btn-bar-open-dir">📂 Explorer</button>
              <button type="button" class="btn sm" id="btn-bar-dismiss">✕</button>`);
            $('btn-bar-open-site').onclick = () => {
              const tab = getActiveTab() || createTab();
              navigateTab(tab, openAddr, { passwords: pws });
            };
            $('btn-bar-save-mysites').onclick = () => {
              const siteTitle = useDemo ? 'Site Demo BrightWeb' : (folder.split(/[\\/]/).filter(Boolean).pop() || 'Meu Site');
              openDNSEditModal({
                kind: 'site',
                name: siteTitle,
                source: openAddr,
                passwords: pws,
                parent_id: 'meus-sites',
                favorite: false,
              });
            };
            $('btn-bar-open-dir').onclick = () => api('/api/system/open-folder', { method: 'POST', body: JSON.stringify({ path: outP }) });
            $('btn-bar-dismiss').onclick = () => $('pub-bottom-bar').classList.add('hidden');
            toast('✅ Vídeo do site gerado com sucesso!', 4000);
            return;
          } else if (j.status === 'cancelled') {
            pubPollActive = false;
            activePubJobId = null;
            setPubBottomBar('🛑', 'Geração cancelada pelo usuário (arquivo parcial removido).', 0,
              '<button type="button" class="btn sm" id="btn-bar-dismiss">✕ Fechar</button>');
            $('btn-bar-dismiss').onclick = () => $('pub-bottom-bar').classList.add('hidden');
            toast('🛑 Geração de vídeo cancelada!', 3000);
            return;
          } else if (j.status === 'error') {
            pubPollActive = false;
            activePubJobId = null;
            setPubBottomBar('❌', `Erro: ${j.error || 'falha desconhecida'}`, 0,
              '<button type="button" class="btn sm" id="btn-bar-dismiss">✕ Fechar</button>');
            $('btn-bar-dismiss').onclick = () => $('pub-bottom-bar').classList.add('hidden');
            toast('❌ ' + (j.error || 'Erro gerando vídeo'), 6000);
            return;
          }
        } catch (err) {
          pubErrs++;
          if (pubErrs >= 10) {
            pubPollActive = false;
            activePubJobId = null;
            setPubBottomBar('❌', `Conexão perdida: ${err.message}`, 0,
              '<button type="button" class="btn sm" id="btn-bar-dismiss">✕ Fechar</button>');
            $('btn-bar-dismiss').onclick = () => $('pub-bottom-bar').classList.add('hidden');
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

  // =========================================================================
  // AGENDA DNS — "MINHA LISTA" (Pastas, Subpastas, Site dentro de Site,
  // Ordenação, Drag-and-Drop, Edição, Ver/Copiar Senha & Link, Importar/Exportar)
  // =========================================================================
  const dnsEntries = () => (state.dnsBook && Array.isArray(state.dnsBook.entries)) ? state.dnsBook.entries : [];

  const getDNSChildren = (parentId) => {
    const kids = dnsEntries().filter((e) => (e.parent_id || '') === (parentId || ''));
    kids.sort((a, b) => {
      if (a.id === 'meus-sites') return -1;
      if (b.id === 'meus-sites') return 1;
      if ((a.order || 0) !== (b.order || 0)) return (a.order || 0) - (b.order || 0);
      return (a.name || '').localeCompare(b.name || '');
    });
    return kids;
  };

  const getDNSDescendantsSet = (rootId) => {
    const out = new Set([rootId]);
    let changed = true;
    while (changed) {
      changed = false;
      for (const e of dnsEntries()) {
        if (!out.has(e.id) && e.parent_id && out.has(e.parent_id)) {
          out.add(e.id);
          changed = true;
        }
      }
    }
    return out;
  };

  const populateDNSParentSelect = (selectEl, excludeId = '', selectedParentId = '') => {
    if (!selectEl) return;
    const blocked = excludeId ? getDNSDescendantsSet(excludeId) : new Set();
    const opts = ['<option value="">📒 Raiz de Minha Lista</option>'];
    const walk = (pid, depth) => {
      for (const e of getDNSChildren(pid)) {
        if (blocked.has(e.id)) continue;
        const pad = '\u00A0\u00A0'.repeat(depth + 1);
        const ico = e.id === 'meus-sites' ? '🏠' : (e.kind === 'folder' ? '📁' : '🌐');
        opts.push(`<option value="${esc(e.id)}">${pad}${ico} ${esc(e.name)}${e.kind === 'site' ? ' (site)' : ''}</option>`);
        walk(e.id, depth + 1);
      }
    };
    walk('', 0);
    selectEl.innerHTML = opts.join('');
    selectEl.value = selectedParentId || '';
  };

  const openDNSEntry = (entry) => {
    if (!entry || entry.kind !== 'site' || !entry.source) return;
    const tab = getActiveTab() || createTab();
    const target = entry.alias ? `btweb://${entry.alias}` : entry.source;
    navigateTab(tab, target, { passwords: entry.passwords || [] });
  };

  const renderDNSTree = () => {
    const tree = $('dns-tree');
    if (!tree) return;
    const q = (state.dnsSearch || '').trim().toLowerCase();
    const favOnly = !!state.dnsFavOnly;
    const all = dnsEntries();

    const matchesFilter = (e) => {
      if (favOnly && !e.favorite) return false;
      if (!q) return true;
      return (e.name || '').toLowerCase().includes(q) ||
        (e.alias || '').toLowerCase().includes(q) ||
        (e.source || '').toLowerCase().includes(q) ||
        (e.notes || '').toLowerCase().includes(q);
    };

    // When filtering, show flat matching results; otherwise render full interactive recursive tree!
    if (q || favOnly) {
      const matched = all.filter((e) => matchesFilter(e));
      if (!matched.length) {
        tree.innerHTML = '<p class="muted" style="font-size:12px;padding:12px;text-align:center">Nenhum item encontrado.</p>';
        return;
      }
      tree.innerHTML = matched.map((e) => renderDNSRowHTML(e, 0, false)).join('');
      return;
    }

    const htmlParts = [];
    const walk = (pid, depth) => {
      const kids = getDNSChildren(pid);
      for (const e of kids) {
        const subKids = getDNSChildren(e.id);
        const hasKids = subKids.length > 0;
        const collapsed = !!state.dnsCollapsed[e.id];
        htmlParts.push(renderDNSRowHTML(e, depth, hasKids, collapsed));
        if (hasKids && !collapsed) {
          walk(e.id, depth + 1);
        }
      }
    };
    walk('', 0);
    tree.innerHTML = htmlParts.join('') || '<p class="muted" style="font-size:12px;padding:12px;text-align:center">Sua lista está vazia. Clique em ➕ Site ou 📁 Pasta!</p>';
  };

  const renderDNSRowHTML = (e, depth, hasKids, collapsed = false) => {
    const isMySites = e.id === 'meus-sites';
    const isSite = e.kind === 'site';
    const hasPw = (e.passwords || []).length > 0;
    const ico = isMySites ? '🏠' : (isSite ? (hasPw ? '🔒' : '🌐') : '📁');
    const padL = 6 + depth * 15;
    const toggleBtn = (hasKids || !isSite)
      ? `<button type="button" class="dns-toggle" data-dns-toggle="${esc(e.id)}" title="${collapsed ? 'Expandir' : 'Recolher'}">${hasKids ? (collapsed ? '▸' : '▾') : '·'}</button>`
      : '<span class="dns-toggle"></span>';
    const aliasHtml = e.alias ? `<span class="dns-alias">btweb://${esc(e.alias)}</span>` : '';
    return `
      <div class="dns-row ${isMySites ? 'is-mysites' : ''} ${e.favorite ? 'is-fav' : ''}"
           style="padding-left:${padL}px"
           draggable="${isMySites ? 'false' : 'true'}"
           data-dns-id="${esc(e.id)}" data-dns-kind="${esc(e.kind)}"
           title="${esc(e.name)}${e.source ? '\n' + e.source : ''}">
        ${toggleBtn}
        <span class="dns-ico">${ico}</span>
        <div class="dns-label-wrap">
          <span class="dns-name">${esc(e.name)}</span>
          ${aliasHtml}
        </div>
        <div class="dns-row-btns">
          ${!isMySites ? `<button type="button" class="dns-mini dns-btn-fav ${e.favorite ? 'fav-on' : ''}" data-dns-fav="${esc(e.id)}" title="Favoritar">${e.favorite ? '★' : '☆'}</button>` : ''}
          ${!isMySites ? `<button type="button" class="dns-mini" data-dns-move="${esc(e.id)}" data-dir="up" title="Mover para cima">↑</button>` : ''}
          ${!isMySites ? `<button type="button" class="dns-mini" data-dns-move="${esc(e.id)}" data-dir="down" title="Mover para baixo">↓</button>` : ''}
          <button type="button" class="dns-mini" data-dns-add-child="${esc(e.id)}" title="Adicionar site dentro de '${esc(e.name)}'">➕</button>
          <button type="button" class="dns-mini" data-dns-edit="${esc(e.id)}" title="Editar / Ver senha / Link de origem">✏️</button>
        </div>
      </div>`;
  };

  let dnsPwVisible = false;
  const renderDNSEditPasswords = (pws) => {
    const list = $('dns-ed-pw-list');
    if (!list) return;
    const arr = (pws && pws.length) ? pws : [''];
    list.innerHTML = arr.map((pw, idx) => `
      <div class="dns-pw-row">
        <input type="${dnsPwVisible ? 'text' : 'password'}" class="dns-ed-pw-in" value="${esc(pw)}" placeholder="Senha ${idx + 1} (opcional)">
        <button type="button" class="btn sm" data-dns-copy-pw="${idx}" title="Copiar esta senha">📋</button>
        <button type="button" class="btn sm" data-dns-del-pw="${idx}" title="Remover campo">✕</button>
      </div>`).join('');
  };

  const syncDNSEditKindUI = () => {
    const isFolder = $('dns-ed-kind').value === 'folder';
    $('dns-ed-alias-wrap').classList.toggle('hidden', isFolder);
    $('dns-ed-source-wrap').classList.toggle('hidden', isFolder);
    $('dns-ed-pw-section').classList.toggle('hidden', isFolder);
  };

  const openDNSEditModal = (entry = {}) => {
    const isNew = !entry.id;
    const isProtected = !!entry.protected || entry.id === 'meus-sites';
    dnsPwVisible = false;
    $('btn-dns-pw-toggle').textContent = '👁️ Ver Senha';
    $('dns-ed-title').textContent = isNew
      ? (entry.kind === 'folder' ? '📁 Nova Pasta — Minha Lista' : '🌐 Salvar Site — Minha Lista')
      : `✏️ Editar — ${entry.name || 'Item'}`;
    $('dns-ed-id').value = entry.id || '';
    $('dns-ed-kind').value = entry.kind === 'folder' ? 'folder' : 'site';
    $('dns-ed-kind').disabled = isProtected;
    populateDNSParentSelect($('dns-ed-parent'), entry.id || '', entry.parent_id || '');
    $('dns-ed-parent').disabled = isProtected;
    $('dns-ed-name').value = entry.name || '';
    $('dns-ed-alias').value = entry.alias || '';
    $('dns-ed-source').value = entry.source || '';
    $('dns-ed-fav').checked = !!entry.favorite;
    $('dns-ed-notes').value = entry.notes || '';
    renderDNSEditPasswords(entry.passwords || []);
    $('btn-dns-delete').classList.toggle('hidden', isNew || isProtected);
    syncDNSEditKindUI();
    $('modal-dns-edit').classList.remove('hidden');
    setTimeout(() => $('dns-ed-name').focus(), 40);
  };
  const closeDNSEditModal = () => $('modal-dns-edit').classList.add('hidden');

  const applyDNSBookUpdate = (book) => {
    if (!book) return;
    state.dnsBook = book;
    renderDNSTree();
    renderHomeLists();
    renderNavBar();
  };

  // Sidebar & Edit Modal Event Bindings
  if ($('btn-dns-sidebar')) {
    $('btn-dns-sidebar').onclick = () => {
      const hidden = $('dns-sidebar').classList.toggle('hidden');
      $('btn-dns-sidebar').classList.toggle('active', !hidden);
      if (!hidden) renderDNSTree();
    };
  }
  if ($('btn-close-dns')) {
    $('btn-close-dns').onclick = () => {
      $('dns-sidebar').classList.add('hidden');
      $('btn-dns-sidebar')?.classList.remove('active');
    };
  }
  if ($('btn-dns-save-cur')) $('btn-dns-save-cur').onclick = () => $('btn-bookmark').click();
  if ($('btn-dns-add-site')) $('btn-dns-add-site').onclick = () => openDNSEditModal({ kind: 'site', parent_id: '' });
  if ($('btn-dns-add-folder')) $('btn-dns-add-folder').onclick = () => openDNSEditModal({ kind: 'folder', parent_id: '' });
  if ($('btn-dns-fav-filter')) {
    $('btn-dns-fav-filter').onclick = () => {
      state.dnsFavOnly = !state.dnsFavOnly;
      $('btn-dns-fav-filter').classList.toggle('active', state.dnsFavOnly);
      renderDNSTree();
    };
  }
  if ($('dns-search')) {
    $('dns-search').oninput = () => {
      state.dnsSearch = $('dns-search').value;
      renderDNSTree();
    };
  }

  // Click & Drag-and-Drop inside #dns-tree (supports folder->folder, site->folder, AND site->site!)
  let draggedDNSId = null;
  if ($('dns-tree')) {
    $('dns-tree').onclick = async (e) => {
      const tog = e.target.closest('[data-dns-toggle]');
      if (tog) {
        e.stopPropagation();
        const id = tog.dataset.dnsToggle;
        state.dnsCollapsed[id] = !state.dnsCollapsed[id];
        renderDNSTree();
        return;
      }
      const favBtn = e.target.closest('[data-dns-fav]');
      if (favBtn) {
        e.stopPropagation();
        try {
          const res = await api('/api/dns/favorite', { method: 'POST', body: JSON.stringify({ id: favBtn.dataset.dnsFav }) });
          applyDNSBookUpdate(res.book);
        } catch (err) { toast('❌ ' + err.message); }
        return;
      }
      const moveBtn = e.target.closest('[data-dns-move]');
      if (moveBtn) {
        e.stopPropagation();
        try {
          const res = await api('/api/dns/reorder', {
            method: 'POST',
            body: JSON.stringify({ id: moveBtn.dataset.dnsMove, direction: moveBtn.dataset.dir }),
          });
          applyDNSBookUpdate(res.book);
        } catch (err) { toast('❌ ' + err.message); }
        return;
      }
      const addChildBtn = e.target.closest('[data-dns-add-child]');
      if (addChildBtn) {
        e.stopPropagation();
        openDNSEditModal({ kind: 'site', parent_id: addChildBtn.dataset.dnsAddChild });
        return;
      }
      const editBtn = e.target.closest('[data-dns-edit]');
      if (editBtn) {
        e.stopPropagation();
        const found = dnsEntries().find((x) => x.id === editBtn.dataset.dnsEdit);
        if (found) openDNSEditModal(found);
        return;
      }
      const row = e.target.closest('[data-dns-id]');
      if (!row) return;
      const item = dnsEntries().find((x) => x.id === row.dataset.dnsId);
      if (!item) return;
      if (item.kind === 'folder') {
        state.dnsCollapsed[item.id] = !state.dnsCollapsed[item.id];
        renderDNSTree();
      } else {
        openDNSEntry(item);
      }
    };

    $('dns-tree').ondragstart = (e) => {
      const row = e.target.closest('[data-dns-id]');
      if (!row || row.dataset.dnsId === 'meus-sites') return;
      draggedDNSId = row.dataset.dnsId;
      e.dataTransfer.effectAllowed = 'move';
      e.dataTransfer.setData('text/plain', draggedDNSId);
    };
    $('dns-tree').ondragover = (e) => {
      if (!draggedDNSId) return;
      e.preventDefault();
      $('dns-tree').querySelectorAll('.drag-over').forEach((el) => el.classList.remove('drag-over'));
      const row = e.target.closest('[data-dns-id]');
      if (row && row.dataset.dnsId !== draggedDNSId) {
        row.classList.add('drag-over');
        $('dns-tree').classList.remove('drop-root-over');
      } else if (!row) {
        $('dns-tree').classList.add('drop-root-over');
      }
    };
    $('dns-tree').ondragleave = () => {
      $('dns-tree').classList.remove('drop-root-over');
    };
    $('dns-tree').ondrop = async (e) => {
      if (!draggedDNSId) return;
      e.preventDefault();
      const srcId = draggedDNSId;
      draggedDNSId = null;
      $('dns-tree').querySelectorAll('.drag-over').forEach((el) => el.classList.remove('drag-over'));
      $('dns-tree').classList.remove('drop-root-over');
      const row = e.target.closest('[data-dns-id]');
      const newParentId = row ? row.dataset.dnsId : '';
      if (newParentId === srcId) return;
      try {
        const res = await api('/api/dns/reorder', {
          method: 'POST',
          body: JSON.stringify({ id: srcId, new_parent_id: newParentId }),
        });
        if (newParentId) state.dnsCollapsed[newParentId] = false;
        applyDNSBookUpdate(res.book);
        toast(newParentId ? '✅ Item movido para dentro do destino!' : '✅ Item movido para a raiz de Minha Lista!');
      } catch (err) { toast('❌ ' + err.message, 4500); }
    };
    $('dns-tree').ondragend = () => {
      draggedDNSId = null;
      $('dns-tree').querySelectorAll('.drag-over').forEach((el) => el.classList.remove('drag-over'));
      $('dns-tree').classList.remove('drop-root-over');
    };
  }

  // Edit Modal Controls
  $('dns-ed-kind').onchange = syncDNSEditKindUI;
  $('btn-close-dns-edit').onclick = closeDNSEditModal;
  $('btn-dns-cancel').onclick = closeDNSEditModal;
  $('btn-dns-pw-toggle').onclick = () => {
    dnsPwVisible = !dnsPwVisible;
    $('btn-dns-pw-toggle').textContent = dnsPwVisible ? '🙈 Ocultar Senha' : '👁️ Ver Senha';
    document.querySelectorAll('#dns-ed-pw-list .dns-ed-pw-in').forEach((inp) => {
      inp.type = dnsPwVisible ? 'text' : 'password';
    });
  };
  $('btn-dns-pw-add').onclick = () => {
    const cur = [...document.querySelectorAll('#dns-ed-pw-list .dns-ed-pw-in')].map((i) => i.value);
    if (cur.length >= 5) return toast('Máximo de 5 senhas em cascata.');
    cur.push('');
    renderDNSEditPasswords(cur);
  };
  $('dns-ed-pw-list').onclick = async (e) => {
    const cp = e.target.closest('[data-dns-copy-pw]');
    if (cp) {
      const inputs = [...document.querySelectorAll('#dns-ed-pw-list .dns-ed-pw-in')];
      const val = inputs[Number(cp.dataset.dnsCopyPw)]?.value || '';
      if (!val) return toast('Campo de senha vazio.');
      try {
        await navigator.clipboard.writeText(val);
        toast('📋 Senha copiada para a área de transferência!');
      } catch { toast('⚠️ Não foi possível copiar automaticamente.'); }
      return;
    }
    const del = e.target.closest('[data-dns-del-pw]');
    if (del) {
      const idx = Number(del.dataset.dnsDelPw);
      const cur = [...document.querySelectorAll('#dns-ed-pw-list .dns-ed-pw-in')].map((i) => i.value);
      cur.splice(idx, 1);
      renderDNSEditPasswords(cur);
    }
  };
  $('btn-dns-open-origin').onclick = async () => {
    const u = $('dns-ed-source').value.trim();
    if (!u) return toast('Informe o link de origem primeiro.');
    try {
      await api('/api/system/open-external', { method: 'POST', body: JSON.stringify({ url: u }) });
      toast('🔗 Abrindo link de origem no navegador externo!');
    } catch (err) { toast('⚠️ ' + err.message, 4500); }
  };
  $('btn-dns-copy-origin').onclick = async () => {
    const u = $('dns-ed-source').value.trim();
    if (!u) return toast('Link vazio.');
    try {
      await navigator.clipboard.writeText(u);
      toast('📋 Link de origem copiado!');
    } catch { toast('⚠️ Não foi possível copiar.'); }
  };
  $('btn-dns-save').onclick = async () => {
    const pws = [...document.querySelectorAll('#dns-ed-pw-list .dns-ed-pw-in')].map((i) => i.value).filter(Boolean);
    const body = {
      id: $('dns-ed-id').value.trim(),
      kind: $('dns-ed-kind').value,
      parent_id: $('dns-ed-parent').value,
      name: $('dns-ed-name').value.trim(),
      alias: $('dns-ed-alias').value.trim(),
      source: $('dns-ed-source').value.trim(),
      passwords: pws,
      favorite: $('dns-ed-fav').checked,
      notes: $('dns-ed-notes').value.trim(),
    };
    try {
      const res = await api('/api/dns/save', { method: 'POST', body: JSON.stringify(body) });
      if (body.parent_id) state.dnsCollapsed[body.parent_id] = false;
      applyDNSBookUpdate(res.book);
      closeDNSEditModal();
      toast('💾 Salvo em Minha Lista!');
    } catch (err) { toast('❌ ' + err.message, 5000); }
  };
  $('btn-dns-delete').onclick = async () => {
    const id = $('dns-ed-id').value.trim();
    if (!id) return;
    const kids = getDNSChildren(id);
    const msg = kids.length
      ? `Deletar este item? Ele possui ${kids.length} filho(s) (clique OK para promover os filhos para fora dele).`
      : 'Tem certeza que deseja deletar este item de Minha Lista?';
    if (!confirm(msg)) return;
    try {
      const res = await api('/api/dns/delete', { method: 'POST', body: JSON.stringify({ id, delete_children: false }) });
      applyDNSBookUpdate(res.book);
      closeDNSEditModal();
      toast('🗑️ Item removido de Minha Lista.');
    } catch (err) { toast('❌ ' + err.message, 5000); }
  };

  // Import / Export Modal
  const openDNSIOModal = () => {
    populateDNSParentSelect($('dns-imp-parent'), '', '');
    $('dns-imp-file').value = '';
    $('dns-imp-url').value = '';
    $('dns-imp-replace').checked = false;
    $('modal-dns-io').classList.remove('hidden');
  };
  const closeDNSIOModal = () => $('modal-dns-io').classList.add('hidden');
  if ($('btn-dns-io')) $('btn-dns-io').onclick = openDNSIOModal;
  if ($('btn-close-dns-io')) $('btn-close-dns-io').onclick = closeDNSIOModal;
  if ($('btn-done-dns-io')) $('btn-done-dns-io').onclick = closeDNSIOModal;
  if ($('btn-dns-export-now')) {
    $('btn-dns-export-now').onclick = () => {
      const inc = $('dns-exp-include-pw').checked ? '1' : '0';
      window.location.href = `/api/dns/export?include_passwords=${inc}`;
      toast('📤 Baixando Minha-Lista-BrightWeb.json!');
    };
  }
  if ($('btn-dns-import-now')) {
    $('btn-dns-import-now').onclick = async () => {
      const file = $('dns-imp-file').files?.[0];
      const url = $('dns-imp-url').value.trim();
      if (!file && !url) return toast('Selecione um arquivo .json/.csv OU cole uma URL pública.');
      let rawContent = '';
      if (file) {
        rawContent = await file.text();
      }
      try {
        const res = await api('/api/dns/import', {
          method: 'POST',
          body: JSON.stringify({
            raw_content: rawContent,
            source_url: file ? '' : url,
            target_parent_id: $('dns-imp-parent').value,
            replace: $('dns-imp-replace').checked,
          }),
        });
        applyDNSBookUpdate(res.book);
        closeDNSIOModal();
        toast(`📥 ${res.imported_count} item(ns) importado(s) para Minha Lista!`, 4000);
      } catch (err) { toast('❌ ' + err.message, 5500); }
    };
  }

  // Keyboard shortcuts
  document.addEventListener('keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 't') { e.preventDefault(); createTab(); }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'l') { e.preventDefault(); $('address-input').focus(); $('address-input').select(); }
    if (e.key === 'Escape') {
      if ($('modal-pub-preview-zoom') && !$('modal-pub-preview-zoom').classList.contains('hidden')) {
        $('modal-pub-preview-zoom').classList.add('hidden');
      } else if ($('modal-dns-edit') && !$('modal-dns-edit').classList.contains('hidden')) {
        closeDNSEditModal();
      } else if ($('modal-dns-io') && !$('modal-dns-io').classList.contains('hidden')) {
        closeDNSIOModal();
      } else if (!$('modal-publish').classList.contains('hidden')) {
        closePublishModal();
      }
    }
  });

  // Persistent SSE keep-alive (immune to Chrome/Edge background tab timer throttling!) + fallback heartbeat
  let bwConnLost = false;
  const showConnLostBanner = (show) => {
    bwConnLost = show;
    const existing = $('bw-conn-lost');
    if (show && !existing) {
      const div = document.createElement('div');
      div.id = 'bw-conn-lost';
      div.style.cssText = 'position:fixed;top:0;left:0;right:0;z-index:9999;background:#dc2626;color:#fff;padding:10px 16px;text-align:center;font-weight:600;font-size:13px;box-shadow:0 4px 16px rgba(0,0,0,.4)';
      div.textContent = '⚠️ Conexão perdida com o processo BrightWeb.exe — feche esta janela e abra o BrightWeb.exe novamente.';
      document.body.prepend(div);
    } else if (!show && existing) {
      existing.remove();
    }
  };
  let bwFailCount = 0;
  const sendHeartbeat = () => {
    fetch('/api/heartbeat', { method: 'POST', credentials: 'same-origin' })
      .then((r) => { if (r.ok) { bwFailCount = 0; if (bwConnLost) showConnLostBanner(false); } })
      .catch(() => { bwFailCount++; if (bwFailCount >= 3) showConnLostBanner(true); });
  };
  if (typeof EventSource !== 'undefined') {
    const es = new EventSource('/api/events', { withCredentials: true });
    es.onopen = () => { bwFailCount = 0; if (bwConnLost) showConnLostBanner(false); };
  }
  setInterval(sendHeartbeat, 15000);
  document.addEventListener('visibilitychange', () => { if (!document.hidden) sendHeartbeat(); });
  sendHeartbeat();

  // Boot
  (async () => {
    try {
      const st = await api('/api/status');
      state.statusMeta = st;
      state.isolated = !!st.isolated_mode;
      $('btn-isolated').classList.toggle('active', state.isolated);
      if (st.dns_book) {
        state.dnsBook = st.dns_book;
        renderDNSTree();
      }
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
