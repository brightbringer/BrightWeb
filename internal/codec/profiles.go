package codec

import "fmt"

// ProfileID uniquely identifies a modulation/block profile inside frame headers.
type ProfileID uint8

const (
	ProfileY2Safe     ProfileID = 1 // 8x8 P&B (1 bit/cell), 30% FEC
	ProfileY2Balanced ProfileID = 2 // 6x6 P&B (1 bit/cell), 25% FEC
	ProfileY4Gray     ProfileID = 3 // 8x8 4-Gray (2 bits/cell, Gray coded), 25% FEC
	ProfileYCbCrDual  ProfileID = 4 // Dual-Grid: Y 8x8 (2 bits) + Cb/Cr 16x16 (2 bits), 30% FEC
	ProfileYCbCrHex   ProfileID = 5 // Hex Uniform: 8x8 (2Y + 1Cb + 1Cr = 4 bits/cell), 30% FEC
	ProfileYCbCrFast  ProfileID = 6 // Dual-Grid High Density: Y 4x4 (2 bits) + Cb/Cr 8x8 (2 bits), 30% FEC
	ProfileY6Gray     ProfileID = 7  // 8x8 6-Gray (2.5 bits/cell, 5 bits per 2 cells), 25% FEC
	ProfileY8Gray     ProfileID = 8  // 8x8 8-Gray (3 bits/cell, Gray coded), 30% FEC
	ProfileArtStego     ProfileID = 9  // Subtractive-Carrier Procedural Art (±12 on 8x8, 1 bit/cell) + Ambient Music, 30% FEC
	ProfileArtChroma    ProfileID = 10 // Orthogonal Chroma Procedural Art + 6-Gray Luma (2.5 bits/cell) + Ambient Music, 25% FEC
	ProfileVideoChroma  ProfileID = 11 // Blind Chroma 4-Colors (16x16 = 8x8 DCT in Cb/Cr) over Host Video Luma, 30% FEC
	ProfileVideoFractal     ProfileID = 12 // Blind Fractal Dream Chroma (±16 over Domain-Warped fBm) over Host Video Luma, 30% FEC
	ProfileVideoBW          ProfileID = 13 // Classic B&W / Grayscale Optical Noise Mask (pure or blind Manchester over Host Video!), 25% FEC
	ProfileArtCrystal       ProfileID = 14 // Cristal 3D — 4 Light Angles DCT(1,0)/DCT(0,1) + 2 DC levels (8x8, 3b/cell, 6.9x AV1/H.264), 25% FEC
	ProfileArtFractal3D     ProfileID = 15 // Fractal 3D Relevo — 4-level subtractive (±48/±16) over 3D Fractal/Theme Luma + Chroma (8x8, 2b/cell, 7.6x AV1), 25% FEC
	ProfileArtAngles        ProfileID = 16 // Linhas & Angulos — 4 Orthogonal Angles (0/45/90/135 deg) + 2 DC levels (8x8, 3b/cell, 11.1x AV1 / 9.4x VP9), 25% FEC
	ProfileArtFractalAngles ProfileID = 17 // Fractal 3D + Linhas/Angulos — Blind 4-Angle engraving over 3D Fractal or Host Video (8x8, 2b/cell, 12.9x H.264), 30% FEC
	ProfileArtBauhaus       ProfileID = 18 // Vitral Bauhaus — 4 Diagonal/Orthogonal Cuts x 2 Polarities (8x8, 3b/cell, 10.1x H.264), 25% FEC
	ProfileArtConstel       ProfileID = 19 // Constelacao / Bokeh — 4 Quadrant Orbs x 2 Polarities (8x8, 3b/cell, 9.2x H.264, 0-3 B VP9), 25% FEC
	ProfileArtTruchet       ProfileID = 20 // Curvas de Truchet — Seamless Continuous Labyrinth Arcs (8x8, 2b/cell, 18.3x H.264), 30% FEC
	ProfileArtFullRes2b     ProfileID = 21 // Arte Full-Res Nitida 2-bit (per-pixel ±40/±14 without flattening 8x8 blocks — ideal for DeepDream & Mandelbrot!), 30% FEC
	ProfileStealthVHS       ProfileID = 22 // Furtivo VHS Analógico — 32x8 horizontal DCT(0,1) scanlines + chroma bleed (zero grade quadrada!), 30% FEC
	ProfileStealthRain      ProfileID = 23 // Furtivo Chuva em Tela Inteira — 22x26 diagonal falling raindrop PPM streaks (2b/gota, zero grade!), 30% FEC
	ProfileStealthFilm      ProfileID = 24 // Furtivo Cinema 35mm — 16x16 Hanning-windowed silver-halide grain (zero grade!), 30% FEC
	ProfileStealthStars     ProfileID = 25 // Furtivo Máscara de Estrelas — 32x30 twinkling starlight overlay (3b/estrela, sem fundo!), 30% FEC
)

type Profile struct {
	ID            ProfileID `json:"id"`
	Code          string    `json:"code"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	YBlockSize    int       `json:"y_block_size"`    // pixels per Y cell
	YLevels       int       `json:"y_levels"`        // 0 (chroma-only), 2 (1b), 4 (2b), 6 (2.5b), or 8 (3b)
	UseChroma     bool      `json:"use_chroma"`      // whether Cb/Cr carry data
	ChromaBlockY  int       `json:"chroma_block_y"`  // luma-space pixels per chroma cell (always even; /2 in 4:2:0 plane)
	ChromaLevels  int       `json:"chroma_levels,omitempty"` // 2 (default: 1b Cb + 1b Cr) or 4 (2b Cb + 2b Cr)
	RSDataBytes   int       `json:"rs_data_bytes"`   // K in RS(255, K)
	RSParityBytes int       `json:"rs_parity_bytes"` // 255 - K
	DefaultCRF    int       `json:"default_crf"`     // Optimal upload CRF for libx264 -preset medium
	ArtMode       string    `json:"art_mode,omitempty"`       // "", "subtractive-y2", "chroma-art"
	GenerateAudio bool      `json:"generate_audio,omitempty"` // synthesize generative ambient soundtrack
}

type Resolution struct {
	Code   string `json:"code"`
	Label  string `json:"label"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

var Resolutions = map[string]Resolution{
	"4k":    {Code: "4k", Label: "4K UHD (3840×2160) — Melhor p/ Arte Generativa", Width: 3840, Height: 2160},
	"2k":    {Code: "2k", Label: "2K QHD (2560×1440) — Equilíbrio Ideal (VP9 + Leve)", Width: 2560, Height: 1440},
	"1080p": {Code: "1080p", Label: "Full HD (1920×1080) — Rápido & Compacto", Width: 1920, Height: 1080},
	"720p":  {Code: "720p", Label: "HD (1280×720) — Menor Arquivo (4,4×) & Menor RAM", Width: 1280, Height: 720},
	"8k":    {Code: "8k", Label: "8K UHD (7680×4320) — Legado", Width: 7680, Height: 4320},
}

var Profiles = map[ProfileID]Profile{
	ProfileY8Gray: {
		ID: ProfileY8Gray, Code: "y8-gray", Name: "8 Tons de Cinza DCT 8×8 (Padrão Recomendado)",
		Description: "PADRÃO DE REFERÊNCIA (3 bits/bloco 8×8 + 30% FEC): gera vídeos ~30% mais curtos que 4 tons. Para cada 1 GB de arquivos: ~5,3 GB no upload e apenas ~6,0 GB no download do YouTube (menor tráfego total). Velocidade: rápida no upload e a mais rápida no download. Segurança: alta em 4K normal (0 erros em VP9 22M), com margem justa sob compressão extrema (12M).",
		YBlockSize: 8, YLevels: 8, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 179, RSParityBytes: 76, DefaultCRF: 30,
	},
	ProfileY6Gray: {
		ID: ProfileY6Gray, Code: "y6-gray", Name: "6 Tons de Cinza DCT 8×8 (Menor Upload / Extra-Seguro)",
		Description: "Em relação ao padrão (8 Tons): arquivo de upload ~4% MENOR (~5,1 GB/GB — sobe um pouco mais rápido), porém download do YouTube ~43% MAIOR (~8,6 GB/GB, pois gera ~11% mais frames). Segurança: SUPERIOR ao padrão — resiste com grande folga até a compressões extremas de 12 Mbps (VP9 e H.264).",
		YBlockSize: 8, YLevels: 6, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 191, RSParityBytes: 64, DefaultCRF: 34,
	},
	ProfileY4Gray: {
		ID: ProfileY4Gray, Code: "y4-gray", Name: "4 Tons de Cinza DCT 8×8 (Clássico Conservador)",
		Description: "Em relação ao padrão (8 Tons): arquivo de upload ~4% maior (~5,5 GB/GB) e download do YouTube ~77% MAIOR (~10,6 GB/GB — ~42% mais frames e mais lento para baixar). Segurança: muito alta em VP9 graças ao grande espaçamento entre os 4 tons (70 níveis), ótimo se preferir máxima simplicidade.",
		YBlockSize: 8, YLevels: 4, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 191, RSParityBytes: 64, DefaultCRF: 38,
	},
	ProfileY2Safe: {
		ID: ProfileY2Safe, Code: "y2-safe", Name: "Ultra-Seguro (Preto e Branco 8×8)",
		Description: "Em relação ao padrão (8 Tons): gera quase 3× mais frames — upload ~35% maior e download do YouTube quase 3× MAIOR (muito mais lento para baixar/processar). Segurança: MÁXIMA absoluta (apenas 2 tons extremos + 30% FEC), feito para backups críticos ou cenários de compressão severa.",
		YBlockSize: 8, YLevels: 2, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 179, RSParityBytes: 76, DefaultCRF: 38,
	},
	ProfileY2Balanced: {
		ID: ProfileY2Balanced, Code: "y2-balanced", Name: "Balanceado (Preto e Branco 6×6)",
		Description: "Em relação ao padrão (8 Tons): gera ~65% mais frames, arquivo de upload bem maior (blocos 6×6 não se alinham à grade 8×8 do YouTube) e download mais lento. Segurança: intermediária — prefira 6 Tons ou 4 Tons 8×8.",
		YBlockSize: 6, YLevels: 2, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 191, RSParityBytes: 64, DefaultCRF: 38,
	},
	ProfileYCbCrHex: {
		ID: ProfileYCbCrHex, Code: "ycbcr-hex", Name: "Colorido Hexadecimal 8×8 (Experimental)",
		Description: "Em relação ao padrão (8 Tons): gera ~26% menos frames (codificação muito rápida), porém arquivo de upload ~15–25% MAIOR (~6,1–6,7 GB/GB) e download do YouTube ~70% MAIOR (~10,2 GB/GB) por causa da cor. Segurança: MENOR que o padrão (sensível ao subsampling de cor 4:2:0 do YouTube).",
		YBlockSize: 8, YLevels: 4, UseChroma: true, ChromaBlockY: 8,
		RSDataBytes: 179, RSParityBytes: 76, DefaultCRF: 38,
	},
	ProfileYCbCrDual: {
		ID: ProfileYCbCrDual, Code: "ycbcr-dual", Name: "Colorido Dual-Grid 8×8/16×16 (Experimental)",
		Description: "Em relação ao padrão (8 Tons): arquivo de upload ~50% MAIOR (~8,2 GB/GB) e mais frames (~26% a mais). Upload e download mais lentos e segurança MENOR que o padrão (cor mais vulnerável no YouTube).",
		YBlockSize: 8, YLevels: 4, UseChroma: true, ChromaBlockY: 16,
		RSDataBytes: 179, RSParityBytes: 76, DefaultCRF: 38,
	},
	ProfileArtStego: {
		ID: ProfileArtStego, Code: "art-stego", Name: "🎨 Arte Generativa + Música Ambiente (Experimento)",
		Description: "EXPERIÊNCIA VISUAL/SONORA (Cancelamento Subtrativo suave ±12 + 30% FEC + trilha pentatônica): gera animações temáticas em tela cheia V2 (Ondas Musicais, Raios, Túnel, Praia, Flora, Aurora) com dados quase invisíveis (PSNR ~26,5 dB). Em relação ao padrão (8 Tons): gera 3× mais frames e arquivo ~2,2× maior; segurança alta em H.264 e VP9 22M.",
		YBlockSize: 8, YLevels: 2, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 179, RSParityBytes: 76, DefaultCRF: 30,
		ArtMode: "subtractive-y2", GenerateAudio: true,
	},
	ProfileArtChroma: {
		ID: ProfileArtChroma, Code: "art-chroma", Name: "🌈 Arte Cromática + 6 Tons + Música Ambiente (Experimento)",
		Description: "EXPERIÊNCIA VISUAL/SONORA COMPACTA (arte colorida nos canais Cb/Cr + 6 tons no brilho Y + 25% FEC + música ambiente): em relação ao padrão (8 Tons), arquivo de upload apenas ~10% maior (~5,8 GB/GB), download ~56% maior (~9,4 GB/GB) e segurança alta em H.264 e VP9 22M.",
		YBlockSize: 8, YLevels: 6, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 191, RSParityBytes: 64, DefaultCRF: 34,
		ArtMode: "chroma-art", GenerateAudio: true,
	},
	ProfileVideoChroma: {
		ID: ProfileVideoChroma, Code: "video-chroma", Name: "🎬 Vídeo Portador — Colorização 4 Cores (Decodificação Cega)",
		Description: "VÍDEO PORTADOR EM COR (Croma 16×16 = grade DCT 8×8 exata + 30% FEC): mantém o vídeo escolhido 100% nítido no brilho (Y em [50..206] sem clipping de preto/branco) e codifica os dados nas 4 cores extremas de Cb/Cr. Decodificação 100% cega (não precisa do vídeo original para extrair!) e 0 B de erro em H.264 e VP9.",
		YBlockSize: 16, YLevels: 0, UseChroma: true, ChromaBlockY: 16,
		RSDataBytes: 179, RSParityBytes: 76, DefaultCRF: 28,
		ArtMode: "video-chroma-q4", GenerateAudio: false,
	},
	ProfileVideoFractal: {
		ID: ProfileVideoFractal, Code: "video-fractal", Name: "🎨 Vídeo Portador — Máscara Artística (Decodificação Cega)",
		Description: "VÍDEO PORTADOR COM MÁSCARA ARTÍSTICA (Chroma ±16 em Cb/Cr + 30% FEC): preserva o brilho original do vídeo e pinta as cores com qualquer estilo escolhido (Sonho Fractal, Raios, Ondas, Praia, Flora, Vórtice ou Aurora). Decodificação 100% cega quanto ao vídeo base (regenera só o estilo pelo ID) e 0 B de erro em H.264 e VP9.",
		YBlockSize: 16, YLevels: 0, UseChroma: true, ChromaBlockY: 16,
		RSDataBytes: 179, RSParityBytes: 76, DefaultCRF: 28,
		ArtMode: "video-fractal-chroma", GenerateAudio: false,
	},
	ProfileVideoBW: {
		ID: ProfileVideoBW, Code: "video-bw", Name: "🏁 Máscara de Ruído P&B Clássico (Estática / 2–8 Tons)",
		Description: "MÁSCARA DE RUÍDO PRETO & BRANCO CLÁSSICO (estilo versões anteriores + suporte a Vídeo Portador e Música!): sem vídeo portador usa blocos 8×8 de ruído P&B/cinza (2 a 8 tons — até 6× mais GBs!); com vídeo portador estampa ruído estático P&B de alto contraste sobre a silhueta do vídeo em decodificação 100% cega.",
		YBlockSize: 8, YLevels: 4, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 191, RSParityBytes: 64, DefaultCRF: 34,
		ArtMode: "video-bw", GenerateAudio: false,
	},
	ProfileArtCrystal: {
		ID: ProfileArtCrystal, Code: "art-crystal", Name: "💎 Cristal 3D — 4 Ângulos DCT1 (6,9× AV1 · 3b/bloco)",
		Description: "CAMPEÃO GEOMÉTRICO (4 ângulos de luz 0°/90°/180°/270° nas frequências mais baixas DCT1 + 2 profundidades + cor fractal): visual de mosaico 3D lapidado com apenas ~6,9× de upload (AV1 ou H.264) e ~7,1× pós-YouTube VP9 (0 B de erro em YT H.264!).",
		YBlockSize: 8, YLevels: 8, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 191, RSParityBytes: 64, DefaultCRF: 34,
		ArtMode: "art-crystal", GenerateAudio: true,
	},
	ProfileArtFractal3D: {
		ID: ProfileArtFractal3D, Code: "art-fractal3d", Name: "☁️ Fractal 3D Relevo (7,6× AV1 · 2× mais leve)",
		Description: "ARTE FRACTAL 3D REAL ULTRA-LEVE (nuvens 3D no brilho + aquarela na cor + relevo subtrativo de 4 níveis ±48/±16): 2× mais denso e quase metade do tamanho da Arte Generativa ±12 (~7,6× AV1 / ~7,2× pós-YouTube VP9).",
		YBlockSize: 8, YLevels: 4, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 191, RSParityBytes: 64, DefaultCRF: 30,
		ArtMode: "art-fractal3d", GenerateAudio: true,
	},
	ProfileArtAngles: {
		ID: ProfileArtAngles, Code: "art-angles", Name: "📐 Linhas & Ângulos — 4 Direções (11× AV1 / 9,4× VP9)",
		Description: "MODULAÇÃO POR LINHAS E ÂNGULOS (0°/45°/90°/135° + 2 tons = 3 bits/bloco 8×8): visual futurista de gravura/circuito geométrico com o menor download VP9 entre os padrões de linhas (~9,4× pós-YouTube!).",
		YBlockSize: 8, YLevels: 8, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 191, RSParityBytes: 64, DefaultCRF: 30,
		ArtMode: "art-angles", GenerateAudio: true,
	},
	ProfileArtFractalAngles: {
		ID: ProfileArtFractalAngles, Code: "art-fractal-angles", Name: "🧬 Fractal 3D + Linhas/Ângulos (12,9× · Decod. Cega)",
		Description: "GRAVURA ANGULAR SOBRE FRACTAL 3D OU VÍDEO (4 ângulos ortogonais gravados de forma 100% cega sobre nuvens fractais 3D ou vídeo portador): zero aspecto de grade quadrada, ~12,9× em H.264 / ~12,2× pós-VP9.",
		YBlockSize: 8, YLevels: 4, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 179, RSParityBytes: 76, DefaultCRF: 30,
		ArtMode: "art-fractal-angles", GenerateAudio: true,
	},
	ProfileArtBauhaus: {
		ID: ProfileArtBauhaus, Code: "art-bauhaus", Name: "🔷 Vitral Bauhaus — Triângulos (10,1× H.264)",
		Description: "MOSAICO DE TRIÂNGULOS & VITRAL GEOMÉTRICO (4 cortes diagonais/ortogonais × 2 polaridades = 3 bits/bloco 8×8 + aquarela fractal): zero quadradinhos, ~10,1× no upload H.264 e 100% validado no YouTube VP9.",
		YBlockSize: 8, YLevels: 8, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 191, RSParityBytes: 64, DefaultCRF: 34,
		ArtMode: "art-bauhaus", GenerateAudio: true,
	},
	ProfileArtConstel: {
		ID: ProfileArtConstel, Code: "art-constel", Name: "✨ Constelação / Bokeh — Pontos (9,2× H.264)",
		Description: "PONTILHISMO & CONSTELAÇÃO ORGÂNICA (pontos luminosos em 4 posições × 2 polaridades = 3 bits/bloco 8×8 + aquarela fractal): ~9,2× no upload H.264 e apenas 0–3 B de erro pós-YouTube VP9!",
		YBlockSize: 8, YLevels: 8, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 191, RSParityBytes: 64, DefaultCRF: 34,
		ArtMode: "art-constel", GenerateAudio: true,
	},
	ProfileArtTruchet: {
		ID: ProfileArtTruchet, Code: "art-truchet", Name: "🌀 Curvas de Truchet — Labirinto Contínuo (18,3× H.264)",
		Description: "LABIRINTO ORGÂNICO 100% CONTÍNUO (arcos de Smith Truchet conectados sem emendas em blocos 8×8 + 30% FEC): visual hipnótico sem grade quadrada, ~18,3× em H.264 e 100% validado em VP9 e H.264.",
		YBlockSize: 8, YLevels: 4, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 179, RSParityBytes: 76, DefaultCRF: 30,
		ArtMode: "art-truchet", GenerateAudio: true,
	},
	ProfileArtFullRes2b: {
		ID: ProfileArtFullRes2b, Code: "art-fullres", Name: "🌌 Arte Full-Res Nítida 2-bit (DeepDream / Mandelbrot · ~6,9×–10×)",
		Description: "NITIDEZ MÁXIMA SEM PIXELAR (relevo por pixel ±40/±14 + 30% FEC): mantém cada espiral do Mandelbrot, ondas DeepDream Layer 2 e orbs Layer 4 em resolução nativa 2K/4K, com ~6,9×–8,0× em H.264 e ~7,7×–8,5× pós-YouTube VP9!",
		YBlockSize: 8, YLevels: 4, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 179, RSParityBytes: 76, DefaultCRF: 32,
		ArtMode: "art-fullres", GenerateAudio: true,
	},
	ProfileStealthVHS: {
		ID: ProfileStealthVHS, Code: "stealth-vhs", Name: "📼 Furtivo: Fita VHS Analógica (Zero Grade · 0% erro H.264/VP9)",
		Description: "MÁSCARA FURTIVA VHS ANALÓGICO (linhas horizontais largas 32×8 + leve sangramento de cor retrô + 30% FEC): passa 100% despercebida como um filtro de fita VHS/TV de tubo sobre qualquer vídeo base ou arte, em tela inteira sem barras e decodificação 100% cega (~18 MB/h em 720p até ~276 MB/h em 4K @ 30 FPS).",
		YBlockSize: 32, YLevels: 2, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 179, RSParityBytes: 76, DefaultCRF: 24,
		ArtMode: "stealth-vhs", GenerateAudio: false,
	},
	ProfileStealthRain: {
		ID: ProfileStealthRain, Code: "stealth-rain", Name: "🌧️ Furtivo: Filtro de Chuva Tela Inteira (2b/gota · Cega)",
		Description: "MÁSCARA FURTIVA DE CHUVA EM TELA INTEIRA (gotas diagonais caindo em 4 trilhas = 2 bits por gota + 30% FEC): cobre 100% do quadro de ponta a ponta sobre qualquer vídeo base ou efeito artístico, sem grade quadrada e com decodificação 100% cega (~18 MB/h em 720p até ~258 MB/h em 4K @ 30 FPS).",
		YBlockSize: 22, YLevels: 4, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 179, RSParityBytes: 76, DefaultCRF: 24,
		ArtMode: "stealth-rain", GenerateAudio: false,
	},
	ProfileStealthFilm: {
		ID: ProfileStealthFilm, Code: "stealth-film", Name: "🎬 Furtivo: Cinema de Rolo 35mm (Grão Orgânico · Zero Grade)",
		Description: "MÁSCARA FURTIVA DE PELÍCULA 35mm (granulação Spread-Spectrum com janela Hanning suave que zera nas bordas + tom quente de cinema + 30% FEC): impossível ver grade quadrada, parece ruído natural de filme Kodak 35mm sobre qualquer vídeo base ou arte (~18 MB/h em 720p até ~276 MB/h em 4K @ 30 FPS).",
		YBlockSize: 16, YLevels: 2, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 179, RSParityBytes: 76, DefaultCRF: 23,
		ArtMode: "stealth-film", GenerateAudio: false,
	},
	ProfileStealthStars: {
		ID: ProfileStealthStars, Code: "stealth-stars", Name: "✨ Furtivo: Máscara de Estrelas Tela Inteira (3b/estrela · 0% erro)",
		Description: "MÁSCARA FURTIVA DE ESTRELAS / BRILHOS (estrelas cintilantes em 8 posições orgânicas = 3 bits por estrela + 30% FEC): máscara pura sem fundo artificial sobre qualquer vídeo base ou efeito, 100% tela inteira, 0% erro em H.264/VP9 e vídeo super leve (~18 MB/h em 720p até ~221 MB/h em 4K @ 30 FPS).",
		YBlockSize: 32, YLevels: 8, UseChroma: false, ChromaBlockY: 0,
		RSDataBytes: 179, RSParityBytes: 76, DefaultCRF: 24,
		ArtMode: "stealth-stars", GenerateAudio: false,
	},
	ProfileYCbCrFast: {
		ID: ProfileYCbCrFast, Code: "ycbcr-fast", Name: "Alta Densidade 4×4 (Uso Local / Avançado)",
		Description: "Em relação ao padrão (8 Tons): gera 4× menos frames (velocidade extrema apenas para uso local), mas o MP4 fica bem pesado e a segurança para YouTube é BAIXA — a recompressão do YouTube costuma destruir blocos 4×4.",
		YBlockSize: 4, YLevels: 4, UseChroma: true, ChromaBlockY: 8,
		RSDataBytes: 179, RSParityBytes: 76, DefaultCRF: 38,
	},
}

func ProfileByCode(code string) (Profile, error) {
	for _, p := range Profiles {
		if p.Code == code {
			return p, nil
		}
	}
	return Profile{}, fmt.Errorf("perfil desconhecido: %s", code)
}

func ResolutionByCode(code string) (Resolution, error) {
	r, ok := Resolutions[code]
	if !ok {
		return Resolution{}, fmt.Errorf("resolução desconhecida: %s", code)
	}
	return r, nil
}

// ListProfiles returns profiles in display order.
func ListProfiles() []Profile {
	order := []ProfileID{
		ProfileY8Gray,
		ProfileY6Gray,
		ProfileY4Gray,
		ProfileArtFullRes2b,
		ProfileStealthStars,
		ProfileStealthRain,
		ProfileStealthVHS,
		ProfileStealthFilm,
		ProfileArtCrystal,
		ProfileArtFractal3D,
		ProfileArtChroma,
		ProfileArtConstel,
		ProfileArtBauhaus,
		ProfileArtAngles,
		ProfileArtFractalAngles,
		ProfileArtTruchet,
		ProfileArtStego,
		ProfileVideoFractal,
		ProfileVideoChroma,
		ProfileVideoBW,
		ProfileY2Safe,
		ProfileY2Balanced,
		ProfileYCbCrHex,
		ProfileYCbCrDual,
		ProfileYCbCrFast,
	}
	out := make([]Profile, 0, len(order))
	for _, id := range order {
		out = append(out, Profiles[id])
	}
	return out
}

// Legacy V1 geometry constants (multiples of 16; kept for 100% backwards-compatible decoding of YTC1 videos).
const (
	FrameMargin   = 48
	HeaderHeight  = 96
	DataTopY      = 160 // Margin(48) + Header(96) + Gap(16) -> 10*16 exact!
	DataBottomPad = 160 // Symmetric 10*16 bottom clearance
)

// Full-Frame V2 geometry constants (96.2% active 4K area = +15.9% more data/frame in 4K, +38.4% in 1080p!).
// All values are exact multiples of 16 for perfect H.264/VP9 8x8 Luma & Chroma macroblock alignment.
const (
	FrameMarginV2   = 16
	HeaderTopV2     = 16
	HeaderHeightV2  = 32 // 16x32 calibration strip + 8x8 DCT-aligned 3x CRC32 header (only 1.5% of 4K height!)
	DataTopYV2      = 48 // HeaderTopV2(16) + HeaderHeightV2(32) = 3*16 exact!
	DataBottomPadV2 = 16 // Single 16px safety macroblock border, zero wasted bottom bar!
)

// FrameLayoutMargins returns (marginX, topY, bottomPad) for V2 (default) or legacy V1 layouts.
func FrameLayoutMargins(layoutV2 bool) (marginX, topY, bottomPad int) {
	return FrameLayoutMarginsForWidth(1920, layoutV2)
}

// FrameLayoutMarginsForWidth adjusts V2 header height to 48px for 720p (w < 1920) so all 3 CRC32 header copies fit cleanly.
func FrameLayoutMarginsForWidth(w int, layoutV2 bool) (marginX, topY, bottomPad int) {
	return FrameLayoutMarginsWithFlags(w, layoutV2, 0)
}

// FrameLayoutMarginsWithFlags computes active data-grid margins accounting for V2 vs V1 and optional Hidden Header mode.
func FrameLayoutMarginsWithFlags(w int, layoutV2 bool, flags uint8) (marginX, topY, bottomPad int) {
	return FrameLayoutMarginsForProfile(w, layoutV2, flags, false)
}

// FrameLayoutMarginsForProfile computes active data-grid margins; chroma-only hidden headers use
// 32x16 Manchester chroma pairs so the dreamy watercolor stays 100% seamless with zero color washout.
func FrameLayoutMarginsForProfile(w int, layoutV2 bool, flags uint8, chromaOnly bool) (marginX, topY, bottomPad int) {
	if !layoutV2 {
		return FrameMargin, DataTopY, DataBottomPad
	}
	if flags&FlagHiddenHeader != 0 {
		if chromaOnly {
			// 384 Manchester chroma pairs (32x16 px each = 2 bits/pair) -> preserves full watercolor colors!
			switch {
			case w > 0 && w < 1920: // 720p: 39 pairs/row -> 10 rows = 160px -> topY = 176
				return FrameMarginV2, HeaderTopV2 + 160, DataBottomPadV2
			case w < 2560: // 1080p: 59 pairs/row -> 7 rows = 112px -> topY = 128
				return FrameMarginV2, HeaderTopV2 + 112, DataBottomPadV2
			case w < 3840: // 2K: 79 pairs/row -> 5 rows = 80px -> topY = 96
				return FrameMarginV2, HeaderTopV2 + 80, DataBottomPadV2
			default: // 4K+: 119 pairs/row -> 4 rows = 64px -> topY = 80
				return FrameMarginV2, HeaderTopV2 + 64, DataBottomPadV2
			}
		}
		// Manchester luma hidden header: 768 pairs (16x8 px each = 1 bit/pair)
		switch {
		case w > 0 && w < 1920: // 720p: 78 cols -> 80px band -> topY = 96
			return FrameMarginV2, HeaderTopV2 + 80, DataBottomPadV2
		case w < 2560: // 1080p: 118 cols -> 64px band -> topY = 80
			return FrameMarginV2, HeaderTopV2 + 64, DataBottomPadV2
		case w < 3840: // 2K: 158 cols -> 48px band -> topY = 64
			return FrameMarginV2, HeaderTopV2 + 48, DataBottomPadV2
		default: // 4K+: 238 cols -> 32px band -> topY = 48
			return FrameMarginV2, HeaderTopV2 + 32, DataBottomPadV2
		}
	}
	if w > 0 && w < 1920 {
		// 720p (1280px wide): 48px header (6 rows x 138 cols = 828 >= 768 bits) -> topY = 64 (4*16 exact!)
		return FrameMarginV2, HeaderTopV2 + 48, DataBottomPadV2
	}
	return FrameMarginV2, DataTopYV2, DataBottomPadV2
}

// EffectiveProfile returns a profile adjusted for the chosen MaskIntensity (0=Balanced, 1=Subtle, 2=Strong, 3=Max).
func EffectiveProfile(base Profile, maskIntensity uint8) Profile {
	p := base
	switch maskIntensity & 3 {
	case MaskIntensitySubtle:
		switch base.ArtMode {
		case "subtractive-y2":
			p.YBlockSize = 16
			p.YLevels = 2
		case "video-fractal-chroma", "video-chroma-q4":
			p.ChromaBlockY = 24 // 12x12 in U/V plane (~0.44x density, fits >=1 RS block even in 720p + hidden header!)
			p.ChromaLevels = 2
		case "chroma-art":
			p.YBlockSize = 16
			p.YLevels = 6
		case "video-bw":
			p.YBlockSize = 8
			p.YLevels = 2 // Classic pure 2-tone B&W (ultra-safe!)
		case "art-crystal", "art-fractal3d", "art-angles", "art-fractal-angles", "art-bauhaus", "art-constel", "art-truchet", "art-fullres":
			p.YBlockSize = 16 // 2x scaled crisp geometric tiles / subtle full-res (0.25x density, extra-safe & bold art!)
		}
	case MaskIntensityStrong:
		switch base.ArtMode {
		case "subtractive-y2":
			p.YBlockSize = 8
			p.YLevels = 4 // 4-level subtractive (-27,-9,+9,+27) = 2x data/frame
		case "video-fractal-chroma", "video-chroma-q4":
			p.ChromaBlockY = 16
			p.ChromaLevels = 4 // 4-level Cb & Cr (2b+2b = 4b/cell) = 2x data/frame
		case "chroma-art":
			p.YBlockSize = 8
			p.YLevels = 8 // 8-Gray = +20% data/frame
		case "video-bw":
			p.YBlockSize = 8
			p.YLevels = 6 // 6-tone classic grayscale noise
		}
	case MaskIntensityMax:
		switch base.ArtMode {
		case "subtractive-y2":
			p.YBlockSize = 4
			p.YLevels = 2 // 4x4 subtractive = 4x data/frame
		case "video-fractal-chroma", "video-chroma-q4":
			p.ChromaBlockY = 8 // 4x4 in U/V plane = 4x data/frame
			p.ChromaLevels = 2
		case "chroma-art":
			p.YBlockSize = 4
			p.YLevels = 2 // 4x4 P&B = +60% data/frame
		case "video-bw":
			p.YBlockSize = 8
			p.YLevels = 8 // 8-tone high-density classic grayscale noise!
		}
	}
	return p
}

// RawByteCapacityLayout returns total raw bytes available inside the data grid before FEC for V1 or V2 layout.
func (p Profile) RawByteCapacityLayout(w, h int, layoutV2 bool) int {
	return p.RawByteCapacityLayoutWithFlags(w, h, layoutV2, 0)
}

// RawByteCapacityLayoutWithFlags returns raw capacity accounting for MaskIntensity & HiddenHeader flags.
func (p Profile) RawByteCapacityLayoutWithFlags(w, h int, layoutV2 bool, flags uint8) int {
	if IsStealthArtMode(p.ArtMode) {
		return stealthRawPayloadCapacity(p.ArtMode, w, h, layoutV2, flags)
	}
	ep := EffectiveProfile(p, MaskIntensityFromFlags(flags))
	mx, topY, botPad := FrameLayoutMarginsForProfile(w, layoutV2, flags, p.YLevels == 0)
	dw := w - 2*mx
	dh := h - topY - botPad
	yCols := dw / ep.YBlockSize
	yRows := dh / ep.YBlockSize
	cells := yCols * yRows
	if (ep.ArtMode == "subtractive-y2" || ep.ArtMode == "video-bw") && flags&FlagHostManchester != 0 {
		// Blind Manchester over host video uses horizontal pairs (2 adjacent Y blocks = 1 cell).
		// Use 12x12 for Subtle so 720p + hidden header still fits >=1 full Reed-Solomon block (325 B >= 255 B)!
		mi := MaskIntensityFromFlags(flags)
		switch mi {
		case MaskIntensitySubtle:
			ep.YBlockSize, ep.YLevels = 12, 2
		case MaskIntensityStrong:
			ep.YBlockSize, ep.YLevels = 8, 4
		case MaskIntensityMax:
			ep.YBlockSize, ep.YLevels = 4, 2
		default:
			ep.YBlockSize, ep.YLevels = 8, 2
		}
		yCols = dw / ep.YBlockSize
		yRows = dh / ep.YBlockSize
		cells = (yCols / 2) * yRows
	}
	var totalBits int
	switch ep.YLevels {
	case 0:
		totalBits = 0
	case 8:
		totalBits = cells * 3
	case 6:
		// 2 cells -> 6x6 = 36 states -> 5 bits per pair (2.5 bits/cell)
		totalBits = (cells / 2) * 5
	case 4:
		totalBits = cells * 2
	default:
		totalBits = cells
	}
	if ep.UseChroma {
		cCols := dw / ep.ChromaBlockY
		cRows := dh / ep.ChromaBlockY
		bitsPerCell := 2
		if ep.ChromaLevels == 4 {
			bitsPerCell = 4
		}
		totalBits += cCols * cRows * bitsPerCell
	}
	return totalBits / 8
}

// RawByteCapacity returns total raw bytes available inside the V2 full-frame data grid before FEC.
func (p Profile) RawByteCapacity(w, h int) int {
	return p.RawByteCapacityLayout(w, h, true)
}

// PayloadCapacityLayout returns usable payload bytes per frame AFTER Reed-Solomon FEC for V1 or V2 layout.
func (p Profile) PayloadCapacityLayout(w, h int, layoutV2 bool) int {
	return p.PayloadCapacityLayoutWithFlags(w, h, layoutV2, 0)
}

// PayloadCapacityLayoutWithFlags returns usable payload bytes per frame accounting for MaskIntensity & HiddenHeader flags.
func (p Profile) PayloadCapacityLayoutWithFlags(w, h int, layoutV2 bool, flags uint8) int {
	ep := EffectiveProfile(p, MaskIntensityFromFlags(flags))
	rawBytes := p.RawByteCapacityLayoutWithFlags(w, h, layoutV2, flags)
	blocks := rawBytes / 255
	return blocks * ep.RSDataBytes
}

// PayloadCapacity returns usable payload bytes per frame AFTER Reed-Solomon FEC (V2 layout).
func (p Profile) PayloadCapacity(w, h int) int {
	return p.PayloadCapacityLayout(w, h, true)
}
