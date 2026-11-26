package browser

import (
	"time"

	"ytcloud/internal/packer"
)

// BuildDemoSiteFiles constructs a rich, self-contained, multi-page educational white-hat demo website
// (HTML + CSS + JS + interactive optical playground + ytweb:// inter-video links) ready to be encoded
// into a 2-to-5 second optical video and opened inside YTBrowser.
func BuildDemoSiteFiles() []packer.InputFile {
	now := time.Now().Format("2006-01-02 15:04:05")
	return []packer.InputFile{
		{
			RelPath: "index.html",
			ModTime: now,
			Data:    []byte(demoIndexHTML),
		},
		{
			RelPath: "security.html",
			ModTime: now,
			Data:    []byte(demoSecurityHTML),
		},
		{
			RelPath: "style.css",
			ModTime: now,
			Data:    []byte(demoStyleCSS),
		},
		{
			RelPath: "app.js",
			ModTime: now,
			Data:    []byte(demoAppJS),
		},
	}
}

const demoStyleCSS = `:root {
  --bg: #070b16;
  --card: #10182d;
  --border: #22325a;
  --text: #e6eefc;
  --muted: #8ea2cc;
  --accent: #38bdf8;
  --accent2: #a78bfa;
  --ok: #34d399;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
body {
  font-family: 'Segoe UI', system-ui, sans-serif;
  background: radial-gradient(circle at 15% 10%, #152348 0%, #070b16 62%);
  color: var(--text);
  min-height: 100vh;
  padding: 28px 18px 48px;
  line-height: 1.55;
}
.wrap { max-width: 920px; margin: 0 auto; }
.nav {
  display: flex; align-items: center; justify-content: space-between; gap: 12px;
  padding: 12px 16px; border-radius: 12px; background: rgba(16,24,45,.88);
  border: 1px solid var(--border); margin-bottom: 22px; flex-wrap: wrap;
}
.brand { font-weight: 800; font-size: 16px; display: flex; align-items: center; gap: 8px; }
.pill {
  font-size: 11px; padding: 2px 9px; border-radius: 99px; font-weight: 700;
  background: linear-gradient(90deg, var(--accent), var(--accent2)); color: #051021;
}
.nav-links { display: flex; gap: 10px; }
.nav-links a {
  color: var(--text); text-decoration: none; font-size: 13px; font-weight: 600;
  padding: 6px 11px; border-radius: 8px; border: 1px solid var(--border);
  background: rgba(56,189,248,.08);
}
.nav-links a:hover, .nav-links a.active { border-color: var(--accent); color: var(--accent); }
.hero {
  background: linear-gradient(135deg, rgba(56,189,248,.13), rgba(167,139,250,.12));
  border: 1px solid rgba(56,189,248,.35); border-radius: 16px; padding: 26px; margin-bottom: 18px;
}
.hero h1 { font-size: 28px; margin-bottom: 8px; }
.hero p { color: var(--muted); font-size: 14.5px; }
.grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(260px, 1fr)); gap: 14px; margin-top: 16px; }
.card {
  background: var(--card); border: 1px solid var(--border); border-radius: 14px; padding: 18px;
}
.card h3 { font-size: 15.5px; margin-bottom: 8px; color: var(--accent); }
.muted { color: var(--muted); font-size: 13px; }
.btn {
  display: inline-flex; align-items: center; gap: 6px; padding: 8px 14px; border-radius: 9px;
  border: 1px solid var(--accent); background: rgba(56,189,248,.16); color: #e0f2fe;
  font-weight: 600; font-size: 13px; cursor: pointer; text-decoration: none;
}
.btn:hover { background: rgba(56,189,248,.28); }
canvas {
  width: 100%; height: 180px; display: block; border-radius: 10px;
  border: 1px solid var(--border); background: #040711; margin-top: 10px;
}
code {
  background: #050914; border: 1px solid var(--border); padding: 2px 6px;
  border-radius: 6px; font-family: Consolas, monospace; font-size: 12px; color: #7dd3fc;
}
.stat-row { display: flex; gap: 10px; flex-wrap: wrap; margin-top: 12px; }
.stat {
  flex: 1; min-width: 140px; background: #090f20; border: 1px solid var(--border);
  border-radius: 10px; padding: 10px 12px;
}
.stat b { font-size: 18px; color: var(--ok); display: block; }
`

const demoIndexHTML = `<!DOCTYPE html>
<html lang="pt-BR">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Portal Óptico Demo — BrightWeb</title>
  <link rel="stylesheet" href="style.css">
</head>
<body>
  <div class="wrap">
    <nav class="nav">
      <div class="brand">☀️ BrightWeb Optical Portal <span class="pill">RODA DE UM VÍDEO!</span></div>
      <div class="nav-links">
        <a href="index.html" class="active">🏠 Início</a>
        <a href="security.html">🛡️ Pesquisa White-Hat</a>
      </div>
    </nav>

    <section class="hero">
      <h1>Bem-vindo à Internet dentro de Vídeos!</h1>
      <p>Esta página inteira (HTML, CSS, JavaScript e animação em Canvas) foi <b>decodificada pixel por pixel</b> a partir de um vídeo óptico usando blocos 8×8 alinhados à DCT, correção de erro Reed-Solomon e descompressão Solid Zstd — sem nenhum servidor web tradicional!</p>
      <div class="stat-row">
        <div class="stat"><b id="clock-val">--:--:--</b><span class="muted">JavaScript ativo no sandbox</span></div>
        <div class="stat"><b id="fps-val">60 FPS</b><span class="muted">Canvas 100% local na RAM</span></div>
        <div class="stat"><b>ZERO</b><span class="muted">Servidores externos acessados</span></div>
      </div>
    </section>

    <div class="grid">
      <div class="card">
        <h3>🌀 Laboratório Interativo (JS + Canvas)</h3>
        <p class="muted">Clique no canvas abaixo para gerar pulsos de interferência óptica — provando execução completa de scripts dentro do sandbox isolado.</p>
        <canvas id="demo-canvas" width="420" height="180"></canvas>
      </div>

      <div class="card">
        <h3>🔗 Hiperlinks entre Vídeos (<code>btweb://</code>)</h3>
        <p class="muted">Qualquer site hospedado em vídeo pode apontar para outro vídeo usando o protocolo <code>btweb://ID_DO_VIDEO</code> ou links relativos internos:</p>
        <div style="display:flex;flex-direction:column;gap:8px;margin-top:12px">
          <a class="btn" href="security.html">📄 Abrir página interna (security.html)</a>
          <button class="btn" id="btn-ping-test" type="button">🛡️ Testar Isolamento CSP (tenta acesso externo)</button>
          <div id="ping-result" class="muted" style="font-size:12px"></div>
        </div>
      </div>
    </div>
  </div>
  <script src="app.js"></script>
</body>
</html>`

const demoSecurityHTML = `<!DOCTYPE html>
<html lang="pt-BR">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Pesquisa White-Hat — BrightWeb</title>
  <link rel="stylesheet" href="style.css">
</head>
<body>
  <div class="wrap">
    <nav class="nav">
      <div class="brand">🛡️ Pesquisa White-Hat <span class="pill">ANÁLISE DE SEGURANÇA</span></div>
      <div class="nav-links">
        <a href="index.html">🏠 Início</a>
        <a href="security.html" class="active">🛡️ Pesquisa White-Hat</a>
      </div>
    </nav>

    <section class="hero">
      <h1>Vetores, Defesas e Implicações de Esteganografia em Vídeo</h1>
      <p>Esta segunda página interna demonstra navegação multi-página completa (<code>index.html → security.html</code>) dentro do mesmo vídeo óptico, servida direto da RAM com isolamento estrito.</p>
    </section>

    <div class="grid">
      <div class="card">
        <h3>1. Canal Encoberto (Covert Channel)</h3>
        <p class="muted">Plataformas de vídeo recomprimem streams com H.264/VP9/AV1 (4:2:0). Alinhar a modulação à grade DCT 8×8 + Reed-Solomon 25–30% permite sobreviver à transcodificação com 0 B de erro, mesmo sobre fractais ou vídeos portadores.</p>
      </div>
      <div class="card">
        <h3>2. Isolamento no BrightWeb</h3>
        <p class="muted">Sites abertos no BrightWeb rodam em <code>&lt;iframe sandbox&gt;</code> sem <code>allow-same-origin</code> e com <b>Content-Security-Policy</b> bloqueando conexões externas — impedindo vazamento de IP ou acesso à API local.</p>
      </div>
      <div class="card">
        <h3>3. Contramedidas Possíveis</h3>
        <p class="muted">Sistemas de detecção podem analisar entropia temporal em coeficientes DCT 8×8, correlação de blocos adjacentes (Manchester) ou aplicar perturbações geométricas sutis (micro-crop/resample).</p>
      </div>
    </div>
  </div>
</body>
</html>`

const demoAppJS = `(() => {
  const clk = document.getElementById('clock-val');
  if (clk) {
    const tick = () => { clk.textContent = new Date().toLocaleTimeString('pt-BR'); };
    tick();
    setInterval(tick, 1000);
  }

  const cv = document.getElementById('demo-canvas');
  if (cv) {
    const ctx = cv.getContext('2d');
    const pulses = [{ x: 210, y: 90, r: 8, hue: 195 }];
    cv.addEventListener('click', (e) => {
      const r = cv.getBoundingClientRect();
      const x = ((e.clientX - r.left) / r.width) * cv.width;
      const y = ((e.clientY - r.top) / r.height) * cv.height;
      pulses.push({ x, y, r: 4, hue: (Math.random() * 360) | 0 });
      if (pulses.length > 14) pulses.shift();
    });
    let t = 0;
    const draw = () => {
      t += 0.03;
      ctx.fillStyle = 'rgba(4, 7, 17, 0.22)';
      ctx.fillRect(0, 0, cv.width, cv.height);
      for (let i = 0; i < 36; i++) {
        const x = (i / 35) * cv.width;
        const y = cv.height / 2 + Math.sin(i * 0.35 + t * 2) * 34 * Math.cos(t * 0.7 + i * 0.1);
        ctx.fillStyle = 'hsl(' + ((i * 9 + t * 50) % 360) + ', 85%, 62%)';
        ctx.fillRect(x - 2, y - 2, 4, 4);
      }
      for (const p of pulses) {
        p.r += 1.6;
        if (p.r > 220) p.r = 4;
        ctx.strokeStyle = 'hsla(' + p.hue + ', 90%, 65%, ' + Math.max(0, 1 - p.r / 220) + ')';
        ctx.lineWidth = 2;
        ctx.beginPath();
        ctx.arc(p.x, p.y, p.r, 0, Math.PI * 2);
        ctx.stroke();
      }
      requestAnimationFrame(draw);
    };
    draw();
  }

  const pingBtn = document.getElementById('btn-ping-test');
  const pingRes = document.getElementById('ping-result');
  if (pingBtn && pingRes) {
    pingBtn.onclick = async () => {
      pingRes.textContent = 'Testando conexão externa...';
      try {
        await fetch('https://example.com', { mode: 'no-cors' });
        pingRes.textContent = '⚠️ Modo Isolado está desligado (conexão externa permitida).';
      } catch {
        pingRes.textContent = '✅ Bloqueado pelo Modo Isolado (CSP)! O site não consegue vazar seu IP.';
      }
    };
  }
})();`
