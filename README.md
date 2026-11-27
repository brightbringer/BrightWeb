<p align="center">
  <img src="assets/brightweb.png" alt="BrightWeb Logo" width="128" />
</p>

<h1 align="center">BrightWeb v0.701</h1>

<p align="center">
  <strong>Navegador experimental e publicador de sites estáticos em vídeo óptico (protocolo <code>btweb://</code> · BrightNet/3)</strong><br/>
  Executável único para Windows · Streaming Online Direto por Camadas · Agenda DNS Local · Sandbox White-Hat
</p>

---

## 🔬 Intuito Científico e Pesquisa White-Hat

O **BrightWeb** e o **BrightVault** (ecossistema **BrightNet**) são projetos abertos de pesquisa científica e engenharia experimental dedicados ao estudo da **resistência, integridade e transmissão de dados binários através de canais ruidosos com compressão severa com perdas** — especificamente codecs modernos de vídeo (H.264, VP9 e AV1) sob transcodificação agressiva, redução de bitrate variável (VBR) e subamostragem cromática (`YUV420p`). A investigação abrange teoria da informação aplicada, códigos corretores de erros Reed-Solomon intra-frame e inter-frame (*Cross-Frame RS*), esteganografia óptica resiliente, compressão com dicionários pré-treinados e protocolos de carregamento progressivo inspirados em HTTP/3 e armazenamento endereçável por conteúdo (CAS).

Todo o ecossistema é desenvolvido sob rigorosos princípios **White-Hat**, com código aberto, transparência total e finalidade estritamente educacional e defensiva: o BrightWeb renderiza páginas dentro de um *sandbox* isolado sem `allow-same-origin` e com **CSP Offline ativo por padrão** (impedindo vazamento de IP ou requisições externas não autorizadas), não opera servidores centrais nem mantém listas oficiais de sites de terceiros (a agenda **Minha Lista** é 100% local no dispositivo do usuário), e toda a criptografia (*Argon2id + AES-256-GCM + XChaCha20-Poly1305*) é executada exclusivamente no computador do usuário.

---

## ✨ Novidades da v0.701

- **📡 Streaming Direto Online + Carregamento Progressivo por Camadas:** o BrightWeb agora "assiste" ao vídeo online em tempo real (sem gravar o vídeo no disco), abrindo a **Camada 0** (`index.html`, CSS e JS críticos) ao vivo enquanto imagens e subpáginas chegam em segundo plano — com fallback automático para `yt-dlp` paralelo (`-N 8`).
- **🚀 Protocolos BrightNet/3:**
  - **Zero *Head-of-Line Blocking* (estilo HTTP/3):** arquivos em frames posteriores abrem imediatamente mesmo se um frame anterior falhou e aguarda reparo pelo Cross-Frame RS.
  - **Promoção automática de *Critical Path* do HTML:** analisa `<link>`, `<script>` e `<img>` do `index.html` e coloca recursos críticos (mesmo em subpastas) nos primeiros frames.
  - **Dicionário Web Zstd embutido (`WebDictV1`, estilo Brotli):** reduz HTML/CSS/JS/SVG/JSON em 15% a 45%, mantendo 100% de retrocompatibilidade.
  - **Cache Local por SHA-256 (CAS / ETag):** revisitas e arquivos compartilhados carregam em **0 ms** direto do cache verificado (modo `cas-instant`).
- **📒 Agenda DNS Local — `Minha Lista` (com pasta dedicada `🏠 Meus Sites`):**
  - Organize sites em **pastas, subpastas** ou **site dentro de site** (sem precisar criar pasta), reordene com `↑`/`↓` ou arraste e solte (**Drag-and-Drop**).
  - Resolução DNS local (`btweb://meu-alias` ou `btweb://meu-alias/pagina.html`) com preenchimento automático de senha salva.
  - Modal **Editar** completo: renomear, deletar, favoritar, **ver e copiar senha(s)**, **ver, copiar e clicar no link do YouTube/origem**.
  - **Importar e Exportar:** exporte `.json` (escolhendo incluir ou omitir senhas) e importe de `.json`, `.csv` ou links públicos (Google Sheets, Google Docs, GitHub Raw).
- **📦 Estúdio de Publicação Completo + Barra Inferior em 2º Plano:**
  - Interface ampla em 2 colunas no padrão do BrightVault com prévia animada GIF, 4 predefinições Web, 5 barras calibradas (incluindo detecção real de imagem estática no fundo), otimização WebP Lossless para PNGs, estúdio de música generativa com prévia de áudio, **Blindagem Cross-Frame RS** e **Duração Mandatória** (Site / Vídeo Base / Áudio) com aviso de capacidade insuficiente.
  - Ao gerar o vídeo, o modal fecha e o progresso segue na **barra inferior** com **cancelamento real imediato** (encerra o FFmpeg sem travar o programa!) e botão rápido **🏠 Salvar em Meus Sites**.
- **🔌 Estabilidade em Segundo Plano (SSE `/api/events`):** conexão SSE persistente impede que o processo local feche quando a janela fica minimizada por muito tempo.

---

## Como baixar e usar (Windows)

Você **não precisa instalar Go, Python nem dependências manualmente**:

1. Baixe **`BrightWeb.exe`** diretamente da página de **[Releases](https://github.com/brightbringer/BrightWeb/releases)** (ou clique em **Code → Download ZIP** e extraia a pasta).
2. Dê **duplo clique em `BrightWeb.exe`**.
3. O BrightWeb inicia seu motor local seguro (`127.0.0.1`) e abre automaticamente o navegador desktop com suporte nativo ao protocolo **`btweb://`**.

> **FFmpeg e yt-dlp:** para manter o executável leve, `ffmpeg` e `yt-dlp` não são embutidos no `.exe`. O BrightWeb usa automaticamente os binários em `%LOCALAPPDATA%\YTCloud\bin` ou no `PATH`.

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
