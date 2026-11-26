<p align="center">
  <img src="assets/brightweb.png" alt="BrightWeb Logo" width="128" />
</p>

<h1 align="center">BrightWeb v0.7</h1>

<p align="center">
  <strong>Navegador descentralizado e publicador de sites estáticos hospedados em vídeo (protocolo <code>btweb://</code>)</strong><br/>
  Executável único para Windows · Streaming Multi-Vídeo (Shards) · Áudio Integrado · Atualização Automática
</p>

---

## Como baixar e usar (Windows)

Você **não precisa instalar Go, Python nem dependências manualmente**:

1. Baixe **`BrightWeb.exe`** diretamente da página de **[Releases](https://github.com/brightbringer/BrightWeb/releases)** (ou clique em **Code → Download ZIP** e extraia a pasta).
2. Dê **duplo clique em `BrightWeb.exe`**.
3. O BrightWeb inicia seu motor local seguro (`127.0.0.1`) e abre automaticamente o navegador desktop com suporte nativo ao protocolo **`btweb://`**.

> **FFmpeg e yt-dlp:** para manter o executável leve, `ffmpeg` e `yt-dlp` não são embutidos no `.exe`. Se ainda não estiverem instalados, o BrightWeb baixa/configura automaticamente ou usa o que já estiver em `%LOCALAPPDATA%\YTCloud\bin` ou no `PATH`.

---

## Principais Recursos

- **Protocolo `btweb://` (compatível também com `ytweb://`):**
  - Abra sites completos (HTML, CSS, JS, imagens, fontes, áudio) diretamente de links do YouTube ou arquivos `.mp4` locais — públicos ou criptografados com senha.
  - Suporte a **carregamento paralelo Multi-Vídeo (Shards)** para abrir páginas e mídias rapidamente.
- **Publicador de Sites Integrado:**
  - Empacote qualquer pasta de site estático em vídeo óptico pronto para o YouTube (vídeo único ou dividido em múltiplos shards paralelos).
  - Modos visuais artísticos/stealth (*Julia-Turing*, *Mandelbrot*, *Stealth Stars*, vídeo base personalizado ou dados puros).
  - **🎵 Trilha de Áudio opcional** ao gerar o site:
    - Sem áudio (silencioso)
    - Música ambiente generativa (com escolha de estilo, clima e prompt)
    - Áudio do vídeo base
    - Arquivo de áudio local (`.mp3`, `.wav`, `.flac`, `.ogg`, `.m4a`, `.aac`)
- **Histórico, Favoritos e Preferências Persistentes:**
  - Histórico de navegação, favoritos, tema e idioma são salvos permanentemente em disco entre sessões.
- **Temas e Idiomas:**
  - Tema padrão **✨ Noite Brilhante**, além de *Noite*, *Papel*, *Deserto*, *Light*, *Rainbow* e *Crepúsculo*, em **9 idiomas** (PT, EN, ES, RU, FR, IT, JA, AR, ZH).
- **Atualizações Automáticas:**
  - Verifica novas versões em `brightbringer/BrightWeb` ao abrir e 1x por dia, com indicador de versão (`v0.7`) na barra de abas e atualização automática em 1 clique.

---

## Como compilar a partir do código-fonte

Requisitos: **Go 1.24+** (Windows 64-bit).

```powershell
.\build.ps1
```

Ou manualmente:

```powershell
$env:CGO_ENABLED = "0"
go build -ldflags="-H=windowsgui" -o BrightWeb.exe ./cmd/ytbrowser
```

---

## Licença

Distribuído sob a licença MIT. Veja [LICENSE](LICENSE) para mais detalhes.
