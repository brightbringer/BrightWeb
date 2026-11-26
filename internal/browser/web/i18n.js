// BrightWeb Internationalization (9 Languages) + 6-Theme Engine
(() => {
  const THEMES = ['bright-night', 'night', 'paper', 'desert', 'light', 'rainbow', 'twilight'];
  const LANGS = ['pt', 'en', 'es', 'ru', 'fr', 'it', 'ja', 'ar', 'zh'];

  const DICT = {
    pt: {
      omni_ph: 'Cole link(s) do YouTube, btweb://ID1+ID2+ID3+ID4 (multi-vídeo) ou .mp4 local...',
      btn_go: 'Ir', btn_isolated: '🛡️ Isolado', btn_open_mode: '⚠️ Aberto',
      btn_inspector: '🔬 Inspetor', btn_publish: '📦 Publicar Site', btn_settings: '⚙️',
      home_badge: 'PROJETO EDUCACIONAL WHITE-HAT · PROTOCOLO BTWEB://',
      home_title: '☀️ BrightWeb — A Internet dentro de Vídeos',
      home_desc: 'Digite ou cole qualquer link de vídeo (ou .mp4 local) na barra acima. O BrightWeb decodifica o brilho das partículas em tempo real, verifica a integridade SHA-256 e renderiza o site completo na RAM dentro de um sandbox isolado.',
      btn_demo: '⚡ Criar & Abrir Site Demo (Vídeo Fractal)', btn_home_pub: '📦 Transformar Meu Site em Vídeo', btn_home_local: '📁 Abrir Vídeo .mp4 Local',
      card_bookmarks: '⭐ Favoritos Ópticos', card_history: '🕘 Histórico Recente', card_how: '🔬 Como Funciona',
      insp_title: '🔬 Inspetor Óptico', pub_title: '📦 Empacotar & Publicar Site em Vídeo Óptico',
      set_title: '⚙️ Aparência & Idioma — BrightWeb', label_theme: '🎨 Tema de Cores', label_lang: '🌐 Idioma',
      theme_bright_night: '✨ Noite Brilhante (Padrão)', theme_night: '🌙 Noite (Azul Escuro)', theme_paper: '📄 Papel Amarelado & Preto', theme_desert: '🏜️ Deserto (Âmbar Escuro)',
      theme_light: '☀️ Light (Branco)', theme_rainbow: '🌈 Rainbow (Arco-Íris Pastel)', theme_twilight: '🌆 Crepúsculo (Violeta)',
      btn_close: 'Fechar', btn_run_pub: '🚀 Gerar Vídeo do Site',
    },
    en: {
      omni_ph: 'Paste YouTube link(s), btweb://ID1+ID2+ID3+ID4 (multi-video), or local .mp4...',
      btn_go: 'Go', btn_isolated: '🛡️ Isolated', btn_open_mode: '⚠️ Open',
      btn_inspector: '🔬 Inspector', btn_publish: '📦 Publish Site', btn_settings: '⚙️',
      home_badge: 'EDUCATIONAL WHITE-HAT PROJECT · BTWEB:// PROTOCOL',
      home_title: '☀️ BrightWeb — The Internet Inside Videos',
      home_desc: 'Type or paste any video link (or local .mp4) in the bar above. BrightWeb decodes optical light particles in real time, verifies SHA-256 integrity, and renders the full website in RAM inside an isolated sandbox.',
      btn_demo: '⚡ Create & Open Demo Site (Fractal Video)', btn_home_pub: '📦 Turn My Site into Video', btn_home_local: '📁 Open Local .mp4 Video',
      card_bookmarks: '⭐ Optical Bookmarks', card_history: '🕘 Recent History', card_how: '🔬 How It Works',
      insp_title: '🔬 Optical Inspector', pub_title: '📦 Package & Publish Site to Optical Video',
      set_title: '⚙️ Appearance & Language — BrightWeb', label_theme: '🎨 Color Theme', label_lang: '🌐 Language',
      theme_night: '🌙 Night (Default)', theme_bright_night: '✨ Bright Night (Midnight Blue & Gold)', theme_paper: '📄 Yellowed Paper & Black', theme_desert: '🏜️ Desert (Dark Amber)',
      theme_light: '☀️ Light (White)', theme_rainbow: '🌈 Rainbow (Pastel)', theme_twilight: '🌆 Twilight (Dusk Violet)',
      btn_close: 'Close', btn_run_pub: '🚀 Generate Site Video',
    },
    es: {
      omni_ph: 'Pega enlace(s) de YouTube, btweb://ID1+ID2 (multi-vídeo) o .mp4 local...',
      btn_go: 'Ir', btn_isolated: '🛡️ Aislado', btn_open_mode: '⚠️ Abierto',
      btn_inspector: '🔬 Inspector', btn_publish: '📦 Publicar Sitio', btn_settings: '⚙️',
      home_badge: 'PROYECTO EDUCATIVO WHITE-HAT · PROTOCOLO BTWEB://',
      home_title: '☀️ BrightWeb — Internet dentro de Vídeos',
      home_desc: 'Escribe o pega cualquier enlace de vídeo (o .mp4 local). BrightWeb decodifica las partículas ópticas en tiempo real, verifica SHA-256 y renderiza el sitio completo en RAM dentro de un sandbox aislado.',
      btn_demo: '⚡ Crear y Abrir Sitio Demo', btn_home_pub: '📦 Convertir Mi Sitio en Vídeo', btn_home_local: '📁 Abrir Vídeo .mp4 Local',
      card_bookmarks: '⭐ Favoritos Ópticos', card_history: '🕘 Historial Reciente', card_how: '🔬 Cómo Funciona',
      insp_title: '🔬 Inspector Óptico', pub_title: '📦 Empaquetar y Publicar Sitio en Vídeo Óptico',
      set_title: '⚙️ Apariencia e Idioma — BrightWeb', label_theme: '🎨 Tema de Colores', label_lang: '🌐 Idioma',
      theme_night: '🌙 Noche (Predeterminado)', theme_bright_night: '✨ Noche Brillante (Azul y Dorado)', theme_paper: '📄 Papel Amarillento y Negro', theme_desert: '🏜️ Desierto (Ámbar Oscuro)',
      theme_light: '☀️ Light (Blanco)', theme_rainbow: '🌈 Rainbow (Pastel)', theme_twilight: '🌆 Crepúsculo (Violeta)',
      btn_close: 'Cerrar', btn_run_pub: '🚀 Generar Vídeo del Sitio',
    },
    ru: {
      omni_ph: 'Вставьте ссылки YouTube, btweb://ID1+ID2 или локальный .mp4...',
      btn_go: 'Перейти', btn_isolated: '🛡️ Изоляция', btn_open_mode: '⚠️ Открыто',
      btn_inspector: '🔬 Инспектор', btn_publish: '📦 Опубликовать сайт', btn_settings: '⚙️',
      home_badge: 'ОБРАЗОВАТЕЛЬНЫЙ ПРОЕКТ WHITE-HAT · ПРОТОКОЛ BTWEB://',
      home_title: '☀️ BrightWeb — Интернет внутри видео',
      home_desc: 'Вставьте ссылку на видео или локальный .mp4. BrightWeb декодирует световые частицы в реальном времени, проверяет SHA-256 и запускает сайт в изолированной песочнице в ОЗУ.',
      btn_demo: '⚡ Создать и открыть демо-сайт', btn_home_pub: '📦 Упаковать сайт в видео', btn_home_local: '📁 Открыть локальный .mp4',
      card_bookmarks: '⭐ Закладки', card_history: '🕘 История', card_how: '🔬 Как это работает',
      insp_title: '🔬 Оптический инспектор', pub_title: '📦 Упаковка сайта в оптическое видео',
      set_title: '⚙️ Тема и язык — BrightWeb', label_theme: '🎨 Цветовая тема', label_lang: '🌐 Язык',
      theme_night: '🌙 Ночь (По умолч.)', theme_bright_night: '✨ Яркая ночь (Синий и Золото)', theme_paper: '📄 Жёлтая бумага и Чёрный', theme_desert: '🏜️ Пустыня (Янтарный)',
      theme_light: '☀️ Светлая (Белая)', theme_rainbow: '🌈 Радуга (Пастель)', theme_twilight: '🌆 Сумерки (Фиолетовый)',
      btn_close: 'Закрыть', btn_run_pub: '🚀 Создать видео сайта',
    },
    fr: {
      omni_ph: 'Collez lien(s) YouTube, btweb://ID1+ID2 ou un fichier .mp4 local...',
      btn_go: 'Aller', btn_isolated: '🛡️ Isolé', btn_open_mode: '⚠️ Ouvert',
      btn_inspector: '🔬 Inspecteur', btn_publish: '📦 Publier un Site', btn_settings: '⚙️',
      home_badge: 'PROJET ÉDUCATIF WHITE-HAT · PROTOCOLE BTWEB://',
      home_title: '☀️ BrightWeb — Internet dans des Vidéos',
      home_desc: 'Collez n’importe quel lien vidéo ou .mp4 local. BrightWeb décode les particules lumineuses en temps réel, vérifie SHA-256 et affiche le site complet en RAM dans un bac à sable isolé.',
      btn_demo: '⚡ Créer & Ouvrir Site Démo', btn_home_pub: '📦 Transformer Mon Site en Vidéo', btn_home_local: '📁 Ouvrir Vidéo .mp4 Locale',
      card_bookmarks: '⭐ Favoris Optiques', card_history: '🕘 Historique Récent', card_how: '🔬 Fonctionnement',
      insp_title: '🔬 Inspecteur Optique', pub_title: '📦 Empaqueter & Publier un Site en Vidéo',
      set_title: '⚙️ Apparence & Langue — BrightWeb', label_theme: '🎨 Thème', label_lang: '🌐 Langue',
      theme_night: '🌙 Nuit (Défaut)', theme_bright_night: '✨ Nuit Brillante (Bleu & Or)', theme_paper: '📄 Papier Jauni & Noir', theme_desert: '🏜️ Désert (Ambre Foncé)',
      theme_light: '☀️ Clair (Blanc)', theme_rainbow: '🌈 Arc-en-ciel (Pastel)', theme_twilight: '🌆 Crépuscule (Violet)',
      btn_close: 'Fermer', btn_run_pub: '🚀 Générer la Vidéo du Site',
    },
    it: {
      omni_ph: 'Incolla link YouTube, btweb://ID1+ID2 o file .mp4 locale...',
      btn_go: 'Vai', btn_isolated: '🛡️ Isolato', btn_open_mode: '⚠️ Aperto',
      btn_inspector: '🔬 Ispettore', btn_publish: '📦 Pubblica Sito', btn_settings: '⚙️',
      home_badge: 'PROGETTO EDUCATIVO WHITE-HAT · PROTOCOLLO BTWEB://',
      home_title: '☀️ BrightWeb — Internet nei Video',
      home_desc: 'Inserisci un link video o un .mp4 locale. BrightWeb decodifica le particelle ottiche in tempo reale, verifica SHA-256 e carica l’intero sito in RAM dentro una sandbox isolata.',
      btn_demo: '⚡ Crea e Apri Sito Demo', btn_home_pub: '📦 Trasforma il Mio Sito in Video', btn_home_local: '📁 Apri Video .mp4 Locale',
      card_bookmarks: '⭐ Preferiti Ottici', card_history: '🕘 Cronologia Recente', card_how: '🔬 Come Funziona',
      insp_title: '🔬 Ispettore Ottico', pub_title: '📦 Impacchetta e Pubblica Sito in Video',
      set_title: '⚙️ Aspetto e Lingua — BrightWeb', label_theme: '🎨 Tema Colori', label_lang: '🌐 Lingua',
      theme_night: '🌙 Notte (Predefinito)', theme_bright_night: '✨ Notte Brillante (Blu e Oro)', theme_paper: '📄 Carta Ingiallita e Nero', theme_desert: '🏜️ Deserto (Ambra Scuro)',
      theme_light: '☀️ Chiaro (Bianco)', theme_rainbow: '🌈 Arcobaleno (Pastello)', theme_twilight: '🌆 Crepuscolo (Viola)',
      btn_close: 'Chiudi', btn_run_pub: '🚀 Genera Video del Sito',
    },
    ja: {
      omni_ph: 'YouTubeリンク、btweb://ID1+ID2、またはローカル .mp4 を入力...',
      btn_go: '開く', btn_isolated: '🛡️ 分離モード', btn_open_mode: '⚠️ 開放',
      btn_inspector: '🔬 インスペクタ', btn_publish: '📦 サイト公開', btn_settings: '⚙️',
      home_badge: 'ホワイトハット教育プロジェクト · BTWEB:// プロトコル',
      home_title: '☀️ BrightWeb — 動画の中のインターネット',
      home_desc: '動画リンクまたはローカル.mp4を入力してください。BrightWebは光学粒子をリアルタイム復号し、SHA-256を検証して安全なサンドボックス内のRAM上でサイトを表示します。',
      btn_demo: '⚡ デモサイトを作成して開く', btn_home_pub: '📦 サイトを動画に変換', btn_home_local: '📁 ローカル .mp4 を開く',
      card_bookmarks: '⭐ ブックマーク', card_history: '🕘 最近の履歴', card_how: '🔬 仕組み',
      insp_title: '🔬 光学インスペクタ', pub_title: '📦 サイトを光学動画にパッケージ',
      set_title: '⚙️ 外観と言語 — BrightWeb', label_theme: '🎨 テーマ', label_lang: '🌐 言語',
      theme_night: '🌙 ナイト (デフォルト)', theme_bright_night: '✨ ブライトナイト (濃紺＆ゴールド)', theme_paper: '📄 生成り紙＆黒', theme_desert: '🏜️ デザート',
      theme_light: '☀️ ライト', theme_rainbow: '🌈 レインボー (パステル)', theme_twilight: '🌆 トワイライト',
      btn_close: '閉じる', btn_run_pub: '🚀 サイト動画を生成',
    },
    ar: {
      omni_ph: 'الصق روابط يوتيوب أو btweb://ID1+ID2 أو ملف .mp4 محلي...',
      btn_go: 'اذهب', btn_isolated: '🛡️ معزول', btn_open_mode: '⚠️ مفتوح',
      btn_inspector: '🔬 الفاحص', btn_publish: '📦 نشر موقع', btn_settings: '⚙️',
      home_badge: 'مشروع تعليمي أمني · بروتوكول BTWEB://',
      home_title: '☀️ BrightWeb — الإنترنت داخل الفيديوهات',
      home_desc: 'الصق أي رابط فيديو أو ملف .mp4 محلي. يفك BrightWeb تشفير الجسيمات الضوئية فورياً ويتحقق من SHA-256 ويعرض الموقع في الذاكرة داخل صندوق رمل معزول.',
      btn_demo: '⚡ إنشاء وفتح موقع تجريبي', btn_home_pub: '📦 تحويل موقعي إلى فيديو', btn_home_local: '📁 فتح فيديو .mp4 محلي',
      card_bookmarks: '⭐ المفضلة الضوئية', card_history: '🕘 السجل الأخير', card_how: '🔬 كيف يعمل',
      insp_title: '🔬 الفاحص الضوئي', pub_title: '📦 حزم ونشر موقع في فيديو ضوئي',
      set_title: '⚙️ المظهر واللغة — BrightWeb', label_theme: '🎨 السمة', label_lang: '🌐 اللغة',
      theme_night: '🌙 ليل (افتراضي)', theme_bright_night: '✨ ليل مشرق (أزرق وذهبي)', theme_paper: '📄 ورق مصفر وأسود', theme_desert: '🏜️ صحراء',
      theme_light: '☀️ فاتح', theme_rainbow: '🌈 قوس قزح (باستيل)', theme_twilight: '🌆 شفق',
      btn_close: 'إغلاق', btn_run_pub: '🚀 إنشاء فيديو الموقع',
    },
    zh: {
      omni_ph: '粘贴 YouTube 链接、btweb://ID1+ID2 或本地 .mp4...',
      btn_go: '前往', btn_isolated: '🛡️ 隔离模式', btn_open_mode: '⚠️ 开放',
      btn_inspector: '🔬 检查器', btn_publish: '📦 发布网站', btn_settings: '⚙️',
      home_badge: '白帽教育研究项目 · BTWEB:// 协议',
      home_title: '☀️ BrightWeb — 视频中的互联网',
      home_desc: '在上方输入任意视频链接或本地 .mp4。BrightWeb 实时解码光学粒子、校验 SHA-256，并在隔离沙盒的内存中渲染完整网站。',
      btn_demo: '⚡ 生成并打开演示网站', btn_home_pub: '📦 将网站打包为视频', btn_home_local: '📁 打开本地 .mp4 视频',
      card_bookmarks: '⭐ 光学书签', card_history: '🕘 最近历史', card_how: '🔬 工作原理',
      insp_title: '🔬 光学检查器', pub_title: '📦 将网站打包发布为光学视频',
      set_title: '⚙️ 外观与语言 — BrightWeb', label_theme: '🎨 配色主题', label_lang: '🌐 语言',
      theme_night: '🌙 夜间 (默认)', theme_bright_night: '✨ 璀璨之夜 (深蓝与金辉)', theme_paper: '📄 复古黄纸与黑墨', theme_desert: '🏜️ 沙漠',
      theme_light: '☀️ 浅色', theme_rainbow: '🌈 彩虹 (粉彩)', theme_twilight: '🌆 暮光',
      btn_close: '关闭', btn_run_pub: '🚀 生成网站视频',
    },
  };

  let curTheme = localStorage.getItem('bv_theme_v3') || 'bright-night';
  if (!THEMES.includes(curTheme)) curTheme = 'bright-night';
  let curLang = localStorage.getItem('bv_lang') || 'pt';
  if (!LANGS.includes(curLang)) curLang = 'pt';

  function applyTheme(theme) {
    if (!THEMES.includes(theme)) theme = 'bright-night';
    curTheme = theme;
    document.documentElement.setAttribute('data-theme', theme);
    try { localStorage.setItem('bv_theme_v3', theme); } catch { }
  }

  function t(key, fallback = '') {
    return (DICT[curLang] && DICT[curLang][key]) || (DICT.pt && DICT.pt[key]) || fallback || key;
  }

  function applyLanguage(lang) {
    if (!LANGS.includes(lang)) lang = 'pt';
    curLang = lang;
    document.documentElement.lang = lang === 'pt' ? 'pt-BR' : lang;
    document.documentElement.dir = lang === 'ar' ? 'rtl' : 'ltr';
    try { localStorage.setItem('bv_lang', lang); } catch { }
    document.querySelectorAll('[data-i18n]').forEach((el) => {
      const val = t(el.dataset.i18n);
      if (val) el.textContent = val;
    });
    document.querySelectorAll('[data-i18n-ph]').forEach((el) => {
      const val = t(el.dataset.i18nPh);
      if (val) el.placeholder = val;
    });
  }

  window.BV_THEME = { apply: applyTheme, current: () => curTheme, list: THEMES };
  window.BV_I18N = { apply: applyLanguage, t, current: () => curLang, list: LANGS };

  applyTheme(curTheme);
  document.addEventListener('DOMContentLoaded', () => {
    applyTheme(curTheme);
    applyLanguage(curLang);
  });
})();
