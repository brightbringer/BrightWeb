package codec

import (
	"encoding/binary"
	"hash/fnv"
	"math"
	"strings"
)

// MusicOptions configures the pure-Go procedural music composer.
type MusicOptions struct {
	Style       string   // "ambient", "lofi", "synthwave", "meditation", "nature", "piano", "chiptune"
	Mood        string   // "dreamy", "calm", "melancholic", "energetic", "mysterious"
	Instruments []string // individual instruments: pad, strings, piano, rhodes, harp, guitar, bells, bowls, bass, drums, solo, rain, ocean, vinyl
	Prompt      string   // Optional user text/theme phrase shaping key, rhythm, progressions & melodic contour
	Seed        uint64
}

// DefaultInstrumentsForStyle returns a curated set of individual instruments for each style (solo is optional!).
func DefaultInstrumentsForStyle(style string) []string {
	switch strings.ToLower(strings.TrimSpace(style)) {
	case "lofi":
		return []string{"rhodes", "bass", "drums", "vinyl"}
	case "synthwave":
		return []string{"pad", "guitar", "bass", "drums"}
	case "meditation":
		return []string{"pad", "bowls", "rain"}
	case "nature":
		return []string{"guitar", "harp", "ocean"}
	case "piano":
		return []string{"piano", "strings", "bass"}
	case "chiptune":
		return []string{"harp", "bass", "drums", "bells"}
	default: // "ambient"
		return []string{"pad", "strings", "bells", "rain"}
	}
}

func hasInstrument(set map[string]bool, name string) bool {
	return set[name]
}

// fastRand32 is a fast deterministic xorshift32 PRNG for noise, vinyl crackle, and Karplus-Strong excitation.
type fastRand32 struct{ s uint32 }

func (r *fastRand32) nextFloat() float64 {
	x := r.s
	if x == 0 {
		x = 0x12345678
	}
	x ^= x << 13
	x ^= x >> 17
	x ^= x << 5
	r.s = x
	return float64(int32(x)) * (1.0 / 2147483648.0)
}

type sectionKind uint8

const (
	secCalm   sectionKind = iota // Soft breathing section: drums drop out, filter warms
	secGroove                    // Main steady accompaniment
	secPeak                      // Fuller arrangement & rhythmic energy
	secSolo                      // Expressive warm solo window (only plays if "solo" is enabled!)
	secBridge                    // Alternate harmonic progression
)

// SynthMusicWAV composes a coherent, warm, evolving stereo WAV (44.1 kHz, 16-bit PCM) using
// chord-tone anchoring, smooth stepwise voice leading (conjunct melody), 14 individually selectable
// instruments, optional warm solo, text/seed-driven diversity, and seamless looping up to 3 minutes.
func SynthMusicWAV(durationSec float64, opts MusicOptions) []byte {
	const sr = 44100
	if durationSec < 1.5 {
		durationSec = 1.5
	}
	seamlessLoop := false
	if durationSec > 180.0 {
		durationSec = 180.0
		seamlessLoop = true
	}

	style := strings.ToLower(strings.TrimSpace(opts.Style))
	if style == "" {
		style = "ambient"
	}
	mood := strings.ToLower(strings.TrimSpace(opts.Mood))
	if mood == "" {
		mood = "dreamy"
	}
	prompt := strings.TrimSpace(opts.Prompt)
	seed := opts.Seed
	if seed == 0 {
		seed = 0x9E3779B97F4A7C15
	}
	if prompt != "" {
		h := fnv.New64a()
		_, _ = h.Write([]byte("ytcloud-music-v2:" + strings.ToLower(prompt)))
		// Mix cleanly with unsigned arithmetic (never signed negative!)
		seed ^= h.Sum64()*0x9E3779B97F4A7C15 + 0x517CC1B727220A95
	}
	// Extra avalanche mix so sequential seeds (e.g. reroll) produce wildly distinct songs!
	mix64 := func(z uint64) uint64 {
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		return z ^ (z >> 31)
	}
	seed = mix64(seed)

	instList := opts.Instruments
	if len(instList) == 0 {
		instList = DefaultInstrumentsForStyle(style)
	}
	inst := make(map[string]bool, len(instList))
	for _, it := range instList {
		k := strings.ToLower(strings.TrimSpace(it))
		if k == "ambience" {
			// Backwards-compatible alias
			if style == "lofi" {
				inst["vinyl"] = true
			} else if style == "nature" {
				inst["ocean"] = true
			} else {
				inst["rain"] = true
			}
			continue
		}
		if k == "pluck" {
			inst["guitar"] = true
			continue
		}
		if k != "" {
			inst[k] = true
		}
	}

	// BPM tailored per style with wide organic seed variation (±10 BPM)
	bpm := 70.0
	switch style {
	case "lofi":
		bpm = 76.0
	case "synthwave":
		bpm = 104.0
	case "meditation":
		bpm = 52.0
	case "nature":
		bpm = 66.0
	case "piano":
		bpm = 72.0
	case "chiptune":
		bpm = 122.0
	default:
		bpm = 62.0
	}
	bpm += float64(int(seed%21)-10) * 0.85
	beatSec := 60.0 / bpm
	barSec := beatSec * 4.0

	if seamlessLoop {
		bars := math.Max(8, math.Round(durationSec/barSec/4.0)*4.0)
		durationSec = bars * barSec
		if durationSec > 184.0 {
			durationSec = math.Floor(180.0/(4.0*barSec)) * 4.0 * barSec
		}
	}

	// Root MIDI note in warm middle-low register (C3=48 .. B3=59)
	rootMidi := 48 + int((seed>>8)%12)

	// Rich bank of 4 distinct, musically tested chord progressions per mood!
	var scale []int
	var progBank [4][4][4]int
	switch mood {
	case "calm": // Warm Major / Add9
		scale = []int{0, 2, 4, 7, 9, 12, 14, 16, 19}
		progBank = [4][4][4]int{
			{{0, 4, 7, 14}, {9, 12, 16, 19}, {5, 9, 12, 16}, {7, 11, 14, 19}}, // I - vi - IV - V
			{{0, 4, 7, 11}, {5, 9, 12, 16}, {9, 12, 16, 19}, {5, 9, 12, 14}}, // Imaj7 - IVmaj7 - vi7 - IVadd9
			{{5, 9, 12, 16}, {0, 4, 7, 14}, {7, 11, 14, 19}, {9, 12, 16, 19}}, // IV - I - V - vi
			{{0, 4, 7, 14}, {4, 7, 11, 14}, {5, 9, 12, 16}, {7, 11, 14, 17}}, // I - iii - IV - V7
		}
	case "melancholic": // Emotional Natural Minor
		scale = []int{0, 2, 3, 5, 7, 8, 10, 12, 14, 15}
		progBank = [4][4][4]int{
			{{0, 3, 7, 14}, {8, 12, 15, 19}, {3, 7, 10, 14}, {10, 14, 17, 22}}, // i - VI - III - VII
			{{0, 3, 7, 10}, {5, 8, 12, 15}, {8, 12, 15, 19}, {7, 10, 14, 17}},  // i - iv - VI - v
			{{8, 12, 15, 19}, {10, 14, 17, 20}, {0, 3, 7, 14}, {0, 3, 7, 10}},  // VI - VII - i - i7
			{{5, 8, 12, 15}, {3, 7, 10, 14}, {8, 12, 15, 19}, {7, 10, 14, 17}}, // iv - III - VI - v
		}
	case "energetic": // Uplifting Dorian / Pop Minor
		scale = []int{0, 2, 3, 5, 7, 9, 10, 12, 14, 17}
		progBank = [4][4][4]int{
			{{0, 3, 7, 10}, {5, 9, 12, 16}, {10, 14, 17, 21}, {7, 10, 14, 17}}, // i - IV - bVII - v
			{{0, 3, 7, 14}, {3, 7, 10, 14}, {5, 9, 12, 16}, {10, 14, 17, 21}},  // i - III - IV - bVII
			{{5, 9, 12, 16}, {7, 10, 14, 17}, {0, 3, 7, 10}, {3, 7, 10, 14}},   // IV - v - i - III
			{{0, 3, 7, 10}, {10, 14, 17, 21}, {5, 9, 12, 16}, {0, 3, 7, 14}},   // i - bVII - IV - i9
		}
	case "mysterious": // Atmospheric Sus2 / Phrygian
		scale = []int{0, 1, 3, 5, 7, 8, 10, 12, 13, 15}
		progBank = [4][4][4]int{
			{{0, 2, 7, 12}, {1, 5, 8, 12}, {10, 14, 17, 20}, {0, 3, 7, 12}}, // isus2 - bII - bvii - i
			{{0, 3, 7, 12}, {8, 12, 15, 19}, {1, 5, 8, 12}, {7, 10, 15, 19}},
			{{1, 5, 8, 12}, {0, 2, 7, 12}, {10, 13, 17, 20}, {0, 3, 7, 12}},
			{{0, 2, 7, 12}, {5, 8, 12, 15}, {1, 5, 8, 12}, {0, 2, 7, 12}},
		}
	default: // "dreamy" — Floating Lydian & Jazz Maj9
		scale = []int{0, 2, 4, 6, 7, 9, 11, 12, 14, 16}
		progBank = [4][4][4]int{
			{{0, 4, 7, 11}, {2, 6, 9, 14}, {9, 12, 16, 19}, {4, 7, 11, 14}}, // Imaj7 - II9 - vi7 - iii7
			{{0, 4, 7, 14}, {4, 7, 11, 14}, {2, 6, 9, 14}, {9, 12, 16, 19}}, // I9 - iii7 - II9 - vi7
			{{9, 12, 16, 19}, {2, 6, 9, 14}, {0, 4, 7, 11}, {4, 7, 11, 14}}, // vi7 - II9 - Imaj7 - iii7
			{{0, 4, 7, 11}, {7, 11, 14, 18}, {9, 12, 16, 19}, {2, 6, 9, 14}},
		}
	}

	// Pick distinct A, B, and Bridge progressions from the 4-progression bank based on seed!
	pAIdx := int((seed >> 16) % 4)
	pBIdx := (pAIdx + 1 + int((seed>>19)%3)) % 4
	pCIdx := (pBIdx + 1 + int((seed>>21)%2)) % 4
	progA, progB, progC := progBank[pAIdx], progBank[pBIdx], progBank[pCIdx]

	// Pick distinct rhythmic groove & accompaniment variants per seed so tracks sound truly different!
	drumPattern := int((seed >> 24) % 4)
	arpPattern := int((seed >> 27) % 4)
	pianoPattern := int((seed >> 30) % 3)

	// Build dynamic musical narrative across sections
	sectionBars := 4.0
	if durationSec <= 20.0 {
		sectionBars = 2.0
	}
	sectionSec := barSec * sectionBars
	numSections := int(math.Ceil(durationSec/sectionSec)) + 1
	sections := make([]sectionKind, numSections)
	for sIdx := 0; sIdx < numSections; sIdx++ {
		if sIdx == 0 {
			if durationSec <= 20.0 {
				sections[sIdx] = secGroove
			} else {
				sections[sIdx] = secCalm
			}
			continue
		}
		h := mix64(seed ^ uint64(sIdx)*0x9E3779B97F4A7C15)
		choices := []sectionKind{secGroove, secPeak, secBridge, secCalm}
		if hasInstrument(inst, "solo") {
			choices = append(choices, secSolo)
		}
		pick := choices[h%uint64(len(choices))]
		if pick == sections[sIdx-1] {
			pick = choices[(h+1)%uint64(len(choices))]
		}
		sections[sIdx] = pick
	}

	sectionAt := func(t float64) (int, sectionKind, float64) {
		idx := int(t / sectionSec)
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sections) {
			idx = len(sections) - 1
		}
		return idx, sections[idx], math.Mod(t, sectionSec) / sectionSec
	}

	chordAt := func(t float64) ([4]int, sectionKind, float64) {
		sIdx, sk, sPos := sectionAt(t)
		bar := int(t/barSec) & 3
		switch sk {
		case secBridge:
			return progC[bar], sk, sPos
		case secPeak, secSolo:
			if sIdx&1 == 1 {
				return progB[bar], sk, sPos
			}
			return progA[bar], sk, sPos
		default:
			return progA[bar], sk, sPos
		}
	}

	// COHERENT MELODY HEURISTIC:
	// Build a smooth stepwise contour (step delta in {-1, 0, +1, +2}) seeded by user prompt + seed.
	// On strong beats, notes snap strictly to chord tones (zero sour clashes!); on weak beats,
	// notes walk smoothly by 1 step in the scale toward the next chord tone!
	promptBytes := []byte(prompt)
	var phraseSteps [16]int
	var phraseRhythm [16]bool
	curDeg := 0
	for i := 0; i < 16; i++ {
		var h uint64
		if len(promptBytes) > 0 {
			b := uint64(promptBytes[i%len(promptBytes)])
			h = mix64(seed ^ (b << uint((i%7)*8)) ^ uint64(i*131))
		} else {
			h = mix64(seed ^ uint64(i+1)*2654435761)
		}
		// Smooth conjunct motion: mostly -1, 0, +1 steps!
		stepDelta := []int{-2, -1, -1, 0, 1, 1, 2}[h%7]
		curDeg += stepDelta
		if curDeg < 0 {
			curDeg = -curDeg
		}
		if curDeg >= len(scale) {
			curDeg = len(scale) - 1 - (curDeg % 3)
		}
		if curDeg < 0 {
			curDeg = 0
		}
		phraseSteps[i] = curDeg
		// Musical breathing: ~60% active notes, always land on beat 1 (i==0) and breathe at phrase end (i==15)
		phraseRhythm[i] = (i == 0) || (i == 8) || (i != 15 && (h>>8)%10 < 6)
	}

	// Pick a guaranteed harmonious MIDI note for step `st` given current chord `ch` and target octave offset.
	coherentNote := func(ch [4]int, step int, baseOctOffset int) int {
		pos := step & 15
		// Strong beats (0, 4, 8, 12) ALWAYS anchor strictly on a chord tone!
		if pos%4 == 0 {
			toneIdx := (phraseSteps[pos] + (step / 4)) & 3
			return rootMidi + baseOctOffset + ch[toneIdx]
		}
		// Passing beats pick the scale tone nearest to the phrase contour (kept within 1 octave of chord)
		return rootMidi + baseOctOffset + scale[phraseSteps[pos]%len(scale)]
	}

	midiFreq := func(m int) float64 {
		return 440.0 * math.Pow(2.0, float64(m-69)/12.0)
	}

	nSamples := int(math.Ceil(durationSec * float64(sr)))
	bufL := make([]float64, nSamples)
	bufR := make([]float64, nSamples)
	invSR := 1.0 / float64(sr)
	rng := fastRand32{s: uint32(seed ^ (seed >> 32) ^ 0xA511E9B3)}

	// --- 1. PAD (Warm analog synth pad) ---
	if hasInstrument(inst, "pad") {
		var phL, phR [4]float64
		var lpL, lpR float64
		baseCut := 0.065
		if style == "synthwave" {
			baseCut = 0.12
		}
		for i := 0; i < nSamples; i++ {
			t := float64(i) * invSR
			ch, sk, sPos := chordAt(t)
			barPos := math.Mod(t, barSec) / barSec
			env := 1.0
			if barPos < 0.09 {
				env = barPos / 0.09
			} else if barPos > 0.91 {
				env = (1.0 - barPos) / 0.09
			}
			cutoff := baseCut
			secGain := 0.16
			if sk == secPeak {
				cutoff = math.Min(0.32, baseCut*1.7)
				secGain = 0.20
			} else if sk == secCalm {
				cutoff = baseCut * (0.65 + 0.3*sPos)
				secGain = 0.19
			}
			lfo := 0.78 + 0.22*fastSin(t*0.16)
			var sL, sR float64
			for v := 0; v < 4; v++ {
				f := midiFreq(rootMidi + ch[v])
				det := 1.0 + 0.0024*float64(v+1)
				phL[v] += f * det * invSR
				phR[v] += f * (2.0 - det) * invSR
				sL += fastSin(phL[v]) + 0.28*fastSin(2*phL[v])
				sR += fastSin(phR[v]) + 0.28*fastSin(2*phR[v])
			}
			lpL += cutoff * (sL*env*lfo*secGain*0.32 - lpL)
			lpR += cutoff * (sR*env*lfo*secGain*0.32 - lpR)
			bufL[i] += lpL
			bufR[i] += lpR
		}
	}

	// --- 2. STRINGS / CELLO (Warm bowed ensemble with slow swell & soft vibrato) ---
	if hasInstrument(inst, "strings") {
		var phL, phR [3]float64
		var lpL, lpR float64
		for i := 0; i < nSamples; i++ {
			t := float64(i) * invSR
			ch, sk, _ := chordAt(t)
			barPos := math.Mod(t, barSec) / barSec
			// Bowed attack & release envelope
			env := math.Sin(barPos * math.Pi)
			env = math.Pow(math.Max(0, env), 0.45)
			vib := 1.0 + 0.0032*fastSin(t*4.6)
			// Cello root (low) + Viola 3rd + Violin 5th
			voicing := [3]int{rootMidi + ch[0] - 12, rootMidi + ch[1], rootMidi + ch[2]}
			var sL, sR float64
			for v := 0; v < 3; v++ {
				f := midiFreq(voicing[v]) * vib
				phL[v] += f * (1.0 + 0.0018*float64(v)) * invSR
				phR[v] += f * (1.0 - 0.0018*float64(v)) * invSR
				// Rich bowed saw-harmonic series (1/k rolloff)
				sL += fastSin(phL[v]) + 0.48*fastSin(2*phL[v]) + 0.26*fastSin(3*phL[v]) + 0.12*fastSin(4*phL[v])
				sR += fastSin(phR[v]) + 0.48*fastSin(2*phR[v]) + 0.26*fastSin(3*phR[v]) + 0.12*fastSin(4*phR[v])
			}
			gain := 0.14
			if sk == secPeak || sk == secBridge {
				gain = 0.18
			}
			lpL += 0.09 * (sL*env*gain*0.28 - lpL)
			lpR += 0.09 * (sR*env*gain*0.28 - lpR)
			bufL[i] += lpL
			bufR[i] += lpR
		}
	}

	// --- 3. BASS (Deep sub / analog synthwave / chiptune triangle) ---
	if hasInstrument(inst, "bass") {
		var phB float64
		var lpB float64
		for i := 0; i < nSamples; i++ {
			t := float64(i) * invSR
			ch, sk, _ := chordAt(t)
			rootNote := rootMidi + ch[0] - 12
			if rootNote < 34 {
				rootNote += 12
			}
			f := midiFreq(rootNote)
			phB += f * invSR
			secAmp := 1.0
			if sk == secCalm {
				secAmp = 0.45
			} else if sk == secPeak {
				secAmp = 1.2
			}
			var bSig float64
			switch style {
			case "synthwave":
				step := math.Mod(t, beatSec*0.5) / (beatSec * 0.5)
				env := math.Exp(-step * 4.0)
				saw := 2.0*(phB-math.Floor(phB)) - 1.0
				bSig = (0.68*fastSin(phB) + 0.32*saw) * env * 0.24 * secAmp
			case "chiptune":
				tri := 2.0*math.Abs(2.0*(phB-math.Floor(phB))-1.0) - 1.0
				bSig = tri * 0.20 * secAmp
			default:
				period := barSec
				if sk == secPeak {
					period = beatSec * 2.0
				}
				pos := math.Mod(t, period) / period
				env := math.Min(1.0, pos*20.0) * math.Exp(-pos*1.6)
				bSig = (0.82*fastSin(phB) + 0.18*fastSin(2*phB)) * env * 0.22 * secAmp
			}
			lpB += 0.11 * (bSig - lpB)
			bufL[i] += lpB
			bufR[i] += lpB
		}
	}

	// --- 4. ACOUSTIC PIANO (Warm cinematic grand piano — coherent chord-tone voicing & arpeggios) ---
	if hasInstrument(inst, "piano") {
		stepDur := beatSec * 0.5
		if pianoPattern == 2 {
			stepDur = beatSec * 1.0
		}
		numEvents := int(math.Ceil(durationSec / stepDur))
		for ev := 0; ev < numEvents; ev++ {
			startSec := float64(ev) * stepDur
			startIdx := int(startSec * float64(sr))
			if startIdx >= nSamples {
				break
			}
			ch, sk, _ := chordAt(startSec)
			stepInBar := ev & 7
			if sk == secCalm && stepInBar%2 == 1 {
				continue
			}
			var notes []int
			switch pianoPattern {
			case 0: // Flowing cinematic broken chord + coherent top melody
				notes = []int{rootMidi + 12 + ch[ev&3]}
				if stepInBar == 0 {
					notes = append(notes, rootMidi+ch[0], rootMidi+ch[2])
				} else if stepInBar == 4 && phraseRhythm[ev&15] {
					notes = append(notes, coherentNote(ch, ev, 12))
				}
			case 1: // Warm block chords on beat 1 & 3 + delicate passing note
				if stepInBar == 0 || stepInBar == 4 {
					notes = []int{rootMidi + ch[0], rootMidi + 12 + ch[1], rootMidi + 12 + ch[2]}
				} else if phraseRhythm[ev&15] {
					notes = []int{coherentNote(ch, ev, 12)}
				}
			default: // Minimalist Erik Satie / Debussy style gentle chords
				if ev%2 == 0 {
					notes = []int{rootMidi + ch[0], rootMidi + 12 + ch[2], rootMidi + 12 + ch[3]}
				} else {
					notes = []int{coherentNote(ch, ev, 12)}
				}
			}
			noteLen := int(math.Min(stepDur*3.2, 3.0) * float64(sr))
			for ni, midiN := range notes {
				if midiN > 78 {
					midiN -= 12 // keep warm, never harsh!
				}
				offset := startIdx + ni*int(math.Round(0.015*float64(sr)))
				if offset >= nSamples {
					continue
				}
				f := midiFreq(midiN)
				pan := 0.32 + 0.36*float64(ni)/float64(len(notes))
				amp := 0.13 / math.Sqrt(float64(len(notes)))
				for j := 0; j < noteLen && offset+j < nSamples; j++ {
					dt := float64(j) * invSR
					att := math.Min(1.0, dt*180.0)
					env := att * math.Exp(-dt*1.85)
					if env < 0.0005 {
						break
					}
					val := (fastSin(f*dt) + 0.36*fastSin(2*f*dt)*math.Exp(-dt*3.0) + 0.14*fastSin(3*f*dt)*math.Exp(-dt*5.5) + 0.04*fastSin(4*f*dt)*math.Exp(-dt*9.0)) * env * amp
					bufL[offset+j] += val * (1.0 - pan*0.5)
					bufR[offset+j] += val * (0.5 + pan*0.5)
				}
			}
		}
	}

	// --- 5. RHODES ELECTRIC PIANO (Warm velvety FM tine chords with gentle stereo tremolo) ---
	if hasInstrument(inst, "rhodes") {
		stepDur := beatSec * 2.0
		numEvents := int(math.Ceil(durationSec / stepDur))
		for ev := 0; ev < numEvents; ev++ {
			startSec := float64(ev) * stepDur
			if style == "lofi" && ev%2 == 1 {
				startSec += beatSec * 0.07
			}
			startIdx := int(startSec * float64(sr))
			if startIdx >= nSamples {
				break
			}
			ch, sk, _ := chordAt(startSec)
			if sk == secCalm && ev%2 == 1 {
				continue
			}
			notes := []int{rootMidi + ch[0], rootMidi + 12 + ch[1], rootMidi + 12 + ch[2], rootMidi + 12 + ch[3]}
			noteLen := int(math.Min(stepDur*1.6, 2.8) * float64(sr))
			for ni, midiN := range notes {
				if midiN > 76 {
					midiN -= 12
				}
				offset := startIdx + ni*int(math.Round(0.022*float64(sr)))
				if offset >= nSamples {
					continue
				}
				f := midiFreq(midiN)
				amp := 0.125 / math.Sqrt(float64(len(notes)))
				for j := 0; j < noteLen && offset+j < nSamples; j++ {
					dt := float64(j) * invSR
					att := math.Min(1.0, dt*140.0)
					env := att * math.Exp(-dt*1.35)
					if env < 0.0005 {
						break
					}
					// Classic mellow FM tine bell + warm sine fundamental + stereo autopan tremolo!
					mod := 0.28 * math.Exp(-dt*7.5) * fastSin(f*7.0*dt)
					val := (fastSin(f*dt+mod) + 0.18*fastSin(2*f*dt)*math.Exp(-dt*4.0)) * env * amp
					trem := 0.5 + 0.25*fastSin((startSec+dt)*2.2)
					bufL[offset+j] += val * (1.0 - trem*0.45)
					bufR[offset+j] += val * (0.55 + trem*0.45)
				}
			}
		}
	}

	// --- 6. ACOUSTIC GUITAR (Warm Karplus-Strong fingerpicked nylon/acoustic patterns) ---
	if hasInstrument(inst, "guitar") {
		stepDur := beatSec * 0.5
		if style == "synthwave" {
			stepDur = beatSec * 0.25
		}
		numEvents := int(math.Ceil(durationSec / stepDur))
		// Classic Travis / fingerpicking arpeggio index patterns
		pickPatterns := [4][8]int{
			{0, 2, 1, 3, 2, 1, 3, 2},
			{0, 1, 2, 3, 2, 1, 0, 2},
			{0, 3, 2, 1, 0, 2, 3, 1},
			{0, 2, 3, 1, 2, 3, 1, 2},
		}
		pat := pickPatterns[arpPattern]
		for ev := 0; ev < numEvents; ev++ {
			startSec := float64(ev) * stepDur
			ch, sk, _ := chordAt(startSec)
			if sk == secCalm && ev%2 == 1 {
				continue
			}
			startIdx := int(startSec * float64(sr))
			if startIdx >= nSamples {
				break
			}
			toneIdx := pat[ev&7]
			midiN := rootMidi + ch[toneIdx]
			if toneIdx > 0 {
				midiN += 12
			}
			if midiN > 74 {
				midiN -= 12
			}
			f := midiFreq(midiN)
			period := int(math.Round(float64(sr) / f))
			if period < 24 {
				period = 24
			}
			if period > 2048 {
				period = 2048
			}
			delayLine := make([]float64, period)
			// Warm nylon-string pluck excitation (double low-pass smoothed)
			p1, p2 := 0.0, 0.0
			for k := 0; k < period; k++ {
				p1 = 0.65*p1 + 0.35*rng.nextFloat()
				p2 = 0.65*p2 + 0.35*p1
				delayLine[k] = p2
			}
			nLen := int(math.Min(stepDur*4.5, 2.2) * float64(sr))
			ptr := 0
			pan := 0.32 + 0.36*float64(toneIdx)/3.0
			for j := 0; j < nLen && startIdx+j < nSamples; j++ {
				nextPtr := ptr + 1
				if nextPtr >= period {
					nextPtr = 0
				}
				sample := delayLine[ptr]
				delayLine[ptr] = 0.5 * (sample + delayLine[nextPtr]) * 0.9962
				ptr = nextPtr
				out := sample * 0.16
				bufL[startIdx+j] += out * (1.0 - pan*0.45)
				bufR[startIdx+j] += out * (0.55 + pan*0.45)
			}
		}
	}

	// --- 7. HARP (Crystalline ascending chord glissandos / 8-bit chiptune pulse) ---
	if hasInstrument(inst, "harp") {
		stepDur := beatSec * 0.5
		if style == "chiptune" {
			stepDur = beatSec * 0.25
		}
		numEvents := int(math.Ceil(durationSec / stepDur))
		for ev := 0; ev < numEvents; ev++ {
			startSec := float64(ev) * stepDur
			ch, sk, _ := chordAt(startSec)
			if style != "chiptune" && !phraseRhythm[ev&15] {
				continue
			}
			if sk == secCalm && ev%3 != 0 {
				continue
			}
			startIdx := int(startSec * float64(sr))
			if startIdx >= nSamples {
				break
			}
			midiN := coherentNote(ch, ev, 12)
			if midiN > 79 {
				midiN -= 12
			}
			f := midiFreq(midiN)
			pan := 0.25 + 0.50*float64(ev&3)/3.0
			if style == "chiptune" {
				nLen := int(stepDur * 0.82 * float64(sr))
				for j := 0; j < nLen && startIdx+j < nSamples; j++ {
					dt := float64(j) * invSR
					sq := -0.085
					if math.Mod(f*dt, 1.0) < 0.25 {
						sq = 0.085
					}
					env := math.Exp(-dt * 4.5)
					bufL[startIdx+j] += sq * env * (1.0 - pan*0.4)
					bufR[startIdx+j] += sq * env * (0.6 + pan*0.4)
				}
			} else {
				// Silky concert harp (clean harmonic pluck with soft bright shimmer)
				nLen := int(2.0 * float64(sr))
				for j := 0; j < nLen && startIdx+j < nSamples; j++ {
					dt := float64(j) * invSR
					att := math.Min(1.0, dt*220.0)
					env := att * math.Exp(-dt*2.4)
					if env < 0.0004 {
						break
					}
					val := (fastSin(f*dt) + 0.28*fastSin(2*f*dt)*math.Exp(-dt*4.5) + 0.08*fastSin(3*f*dt)*math.Exp(-dt*8.0)) * env * 0.11
					bufL[startIdx+j] += val * (1.0 - pan*0.5)
					bufR[startIdx+j] += val * (0.5 + pan*0.5)
				}
			}
		}
	}

	// --- 8. BELLS / MUSIC BOX (Gentle crystalline chord-anchored chimes — never piercing!) ---
	if hasInstrument(inst, "bells") {
		stepDur := beatSec * 1.0
		numEvents := int(math.Ceil(durationSec / stepDur))
		for ev := 0; ev < numEvents; ev++ {
			if !phraseRhythm[ev&15] {
				continue
			}
			startSec := float64(ev) * stepDur
			ch, _, _ := chordAt(startSec)
			startIdx := int(startSec * float64(sr))
			if startIdx >= nSamples {
				break
			}
			midiN := coherentNote(ch, ev, 12)
			if midiN < 64 {
				midiN += 12
			}
			if midiN > 81 { // cap at A5 (~880 Hz) so bells are sweet, never shrill!
				midiN -= 12
			}
			f := midiFreq(midiN)
			pan := 0.22 + 0.56*float64(ev&3)/3.0
			nLen := int(2.1 * float64(sr))
			for j := 0; j < nLen && startIdx+j < nSamples; j++ {
				dt := float64(j) * invSR
				att := math.Min(1.0, dt*110.0)
				env := att * math.Exp(-dt*2.5)
				if env < 0.0004 {
					break
				}
				s := fastSin(f*dt+0.25*math.Exp(-dt*6.0)*fastSin(f*2.0*dt)) + 0.16*fastSin(f*2.756*dt)*math.Exp(-dt*5.0)
				val := s * env * 0.075
				bufL[startIdx+j] += val * (1.0 - pan*0.5)
				bufR[startIdx+j] += val * (0.5 + pan*0.5)
			}
		}
	}

	// --- 9. TIBETAN SINGING BOWLS (Deep, warm, slowly pulsing meditative resonance) ---
	if hasInstrument(inst, "bowls") {
		stepDur := barSec * 1.0
		numEvents := int(math.Ceil(durationSec / stepDur))
		for ev := 0; ev < numEvents; ev++ {
			startSec := float64(ev) * stepDur
			ch, _, _ := chordAt(startSec)
			startIdx := int(startSec * float64(sr))
			if startIdx >= nSamples {
				break
			}
			// Always strike the chord root or 5th in warm low-mid register (MIDI 52..67)
			midiN := rootMidi + ch[(ev&1)*2]
			if midiN > 67 {
				midiN -= 12
			}
			f := midiFreq(midiN)
			pan := 0.35 + 0.30*float64(ev&1)
			nLen := int(4.5 * float64(sr))
			for j := 0; j < nLen && startIdx+j < nSamples; j++ {
				dt := float64(j) * invSR
				att := math.Min(1.0, dt*16.0) // soft mallet bloom
				env := att * math.Exp(-dt*0.85)
				if env < 0.0004 {
					break
				}
				pulse := 0.82 + 0.18*fastSin(dt*2.1)
				s := (fastSin(f*dt) + 0.32*fastSin(f*2.756*dt)*math.Exp(-dt*1.6) + 0.10*fastSin(f*5.404*dt)*math.Exp(-dt*3.2)) * pulse
				val := s * env * 0.12
				bufL[startIdx+j] += val * (1.0 - pan*0.4)
				bufR[startIdx+j] += val * (0.6 + pan*0.4)
			}
		}
	}

	// --- 10. OPTIONAL EXPRESSIVE SOLO (ONLY plays when "solo" is checked! Warm mellow register, NEVER piercing!) ---
	if hasInstrument(inst, "solo") {
		soloStep := beatSec * 0.5
		numSteps := int(math.Ceil(durationSec / soloStep))
		soloBufL := make([]float64, nSamples)
		soloBufR := make([]float64, nSamples)
		for st := 0; st < numSteps; st++ {
			tStart := float64(st) * soloStep
			_, sk, sPos := sectionAt(tStart)
			if sk != secSolo && !(sk == secPeak && sPos > 0.45) && durationSec > 20.0 {
				continue
			}
			if !phraseRhythm[st&15] {
				continue
			}
			startIdx := int(tStart * float64(sr))
			if startIdx >= nSamples {
				break
			}
			ch, _, _ := chordAt(tStart)
			midiN := coherentNote(ch, st, 12)
			// Strictly warm, velvet vocal/flute register (MIDI 57..74 = 220..587 Hz — NEVER harsh!)
			for midiN > 74 {
				midiN -= 12
			}
			for midiN < 57 {
				midiN += 12
			}
			f := midiFreq(midiN)
			durBeats := 1.15
			if (st&7) == 0 || (st&7) == 4 {
				durBeats = 1.9
			}
			nLen := int(durBeats * soloStep * float64(sr))
			var ph float64
			for j := 0; j < nLen && startIdx+j < nSamples; j++ {
				dt := float64(j) * invSR
				att := math.Min(1.0, dt*14.0) // soft 70ms breathy attack
				rel := 1.0
				rem := float64(nLen-j) * invSR
				if rem < 0.10 {
					rel = rem / 0.10
				}
				env := att * rel * math.Exp(-dt*0.9)
				vib := 0.0
				if dt > 0.18 {
					vib = math.Min(0.004, (dt-0.18)*0.012)
				}
				ph += f * (1.0 + vib*fastSin(dt*4.8)) * invSR
				// Soft warm mellow woodwind/electric-flute tone
				tone := fastSin(ph) + 0.22*fastSin(2*ph)*math.Exp(-dt*2.0)
				val := tone * env * 0.085
				soloBufL[startIdx+j] += val * 0.92
				soloBufR[startIdx+j] += val * 1.08
			}
		}
		// Warm low-pass filter on the solo bus so it sits smoothly inside the mix!
		var slpL, slpR float64
		for i := 0; i < nSamples; i++ {
			slpL += 0.10 * (soloBufL[i] - slpL)
			slpR += 0.10 * (soloBufR[i] - slpR)
			bufL[i] += slpL
			bufR[i] += slpR
		}
	}

	// --- 11. DRUMS / GROOVE (4 distinct rhythmic patterns + calm dropouts & peak fills) ---
	if hasInstrument(inst, "drums") {
		stepDur := beatSec * 0.5
		numSteps := int(math.Ceil(durationSec / stepDur))
		kickPats := [4][8]bool{
			{true, false, false, false, false, true, false, false},  // classic boom-bap lo-fi
			{true, false, false, false, true, false, false, false},  // four-on-the-floor / synthwave
			{true, false, false, true, false, false, true, false},   // syncopated chill
			{true, false, true, false, false, true, false, false},   // lively bounce
		}
		snarePats := [4][8]bool{
			{false, false, true, false, false, false, true, false},
			{false, false, true, false, false, false, true, false},
			{false, false, false, false, true, false, false, false}, // half-time dreamy snare!
			{false, false, true, false, false, false, true, true},
		}
		kPat := kickPats[drumPattern]
		sPat := snarePats[drumPattern]
		for st := 0; st < numSteps; st++ {
			tStart := float64(st) * stepDur
			_, sk, sPos := sectionAt(tStart)
			if sk == secCalm {
				continue
			}
			stepInBar := st & 7
			if style == "lofi" && stepInBar&1 == 1 {
				tStart += stepDur * 0.11
			}
			startIdx := int(tStart * float64(sr))
			if startIdx >= nSamples {
				break
			}
			isFill := sPos > 0.90 && stepInBar >= 6
			doKick := kPat[stepInBar]
			doSnare := sPat[stepInBar] || isFill
			doHat := (stepInBar%2 == 0) || sk == secPeak || style == "lofi"

			if doKick {
				kLen := int(0.18 * float64(sr))
				kGain := 0.22
				if sk == secPeak {
					kGain = 0.26
				}
				ph := 0.0
				for j := 0; j < kLen && startIdx+j < nSamples; j++ {
					dt := float64(j) * invSR
					ph += (45.0 + 108.0*math.Exp(-dt*36.0)) * invSR
					v := fastSin(ph) * math.Exp(-dt*18.0) * kGain
					bufL[startIdx+j] += v
					bufR[startIdx+j] += v
				}
			}
			if doSnare {
				sLen := int(math.Round(0.13 * float64(sr)))
				sGain := 0.095
				if style == "synthwave" || sk == secPeak {
					sGain = 0.13
				}
				lp := 0.0
				for j := 0; j < sLen && startIdx+j < nSamples; j++ {
					dt := float64(j) * invSR
					env := math.Exp(-dt * 25.0)
					lp += 0.28 * (rng.nextFloat() - lp)
					body := fastSin(180.0*dt) * math.Exp(-dt*35.0) * 0.35
					v := (lp*0.75 + body) * env * sGain
					bufL[startIdx+j] += v * 0.95
					bufR[startIdx+j] += v * 1.05
				}
			}
			if doHat {
				hLen := int(math.Round(0.040 * float64(sr)))
				hGain := 0.025
				if stepInBar&1 == 0 {
					hGain = 0.036
				}
				prev := 0.0
				for j := 0; j < hLen && startIdx+j < nSamples; j++ {
					dt := float64(j) * invSR
					n := rng.nextFloat()
					hp := n - prev
					prev = n
					v := hp * math.Exp(-dt*70.0) * hGain
					bufL[startIdx+j] += v * 0.85
					bufR[startIdx+j] += v * 1.15
				}
			}
		}
	}

	// --- 12. RAIN (Gentle soft stereo rainfall) ---
	if hasInstrument(inst, "rain") {
		var rL0, rL1, rR0, rR1 float64
		for i := 0; i < nSamples; i++ {
			t := float64(i) * invSR
			wL, wR := rng.nextFloat(), rng.nextFloat()
			rL0 = 0.94*rL0 + 0.06*wL
			rL1 = 0.78*rL1 + 0.22*wL
			rR0 = 0.94*rR0 + 0.06*wR
			rR1 = 0.78*rR1 + 0.22*wR
			swell := 0.80 + 0.20*fastSin(t*0.11)
			bufL[i] += (rL0 + rL1*0.5) * swell * 0.085
			bufR[i] += (rR0 + rR1*0.5) * swell * 0.085
		}
	}

	// --- 13. OCEAN WAVES (Deep stereo rolling beach surf) ---
	if hasInstrument(inst, "ocean") {
		var oL0, oL1, oR0, oR1 float64
		for i := 0; i < nSamples; i++ {
			t := float64(i) * invSR
			wL, wR := rng.nextFloat(), rng.nextFloat()
			oL0 = 0.992*oL0 + 0.008*wL
			oL1 = 0.95*oL1 + 0.05*wL
			oR0 = 0.992*oR0 + 0.008*wR
			oR1 = 0.95*oR1 + 0.05*wR
			swellL := 0.30 + 0.70*math.Pow(0.5+0.5*fastSin(t*0.14), 2.2)
			swellR := 0.30 + 0.70*math.Pow(0.5+0.5*fastSin(t*0.14+0.12), 2.2)
			bufL[i] += (oL0 + oL1*0.6) * swellL * 0.22
			bufR[i] += (oR0 + oR1*0.6) * swellR * 0.22
		}
	}

	// --- 14. VINYL CRACKLE (Warm lo-fi analog dust & gentle hiss) ---
	if hasInstrument(inst, "vinyl") {
		var vL, vR float64
		for i := 0; i < nSamples; i++ {
			wL, wR := rng.nextFloat(), rng.nextFloat()
			vL = 0.92*vL + 0.08*wL
			vR = 0.92*vR + 0.08*wR
			pop := 0.0
			if math.Abs(wL) > 0.9986 {
				pop = wL * 0.075
			}
			bufL[i] += vL*0.032 + pop
			bufR[i] += vR*0.032 + pop*0.65
		}
	}

	// --- 15. STEREO PING-PONG TAPE DELAY / REVERB ---
	if style != "chiptune" {
		d1 := int(beatSec * 0.75 * float64(sr))
		d2 := int(beatSec * 0.50 * float64(sr))
		if d1 < 4000 {
			d1 = 11025
		}
		if d2 < 3000 {
			d2 = 7350
		}
		if d1 > 44100 {
			d1 = 33075
		}
		if d2 > 44100 {
			d2 = 22050
		}
		delL := make([]float64, d1)
		delR := make([]float64, d2)
		wet, fb := 0.24, 0.36
		if style == "ambient" || style == "meditation" {
			wet, fb = 0.35, 0.46
		}
		i1, i2 := 0, 0
		var dLpL, dLpR float64
		for i := 0; i < nSamples; i++ {
			dLpL += 0.24 * (delL[i1] - dLpL)
			dLpR += 0.24 * (delR[i2] - dLpR)
			delL[i1] = bufL[i] + dLpR*fb
			delR[i2] = bufR[i] + dLpL*fb
			bufL[i] += dLpL * wet
			bufR[i] += dLpR * wet
			i1++
			if i1 >= d1 {
				i1 = 0
			}
			i2++
			if i2 >= d2 {
				i2 = 0
			}
		}
	}

	// --- 16. WARM MASTER LOW-PASS + SOFT LIMITER + SEAMLESS FADE ---
	masterCut := 0.65
	if style == "lofi" {
		masterCut = 0.38 // cozy warm lo-fi tape rolloff
	} else if style == "meditation" || style == "ambient" {
		masterCut = 0.50
	}
	fadeSamples := int(0.10 * float64(sr))
	if seamlessLoop {
		fadeSamples = int(0.03 * float64(sr))
	}
	if fadeSamples > nSamples/3 {
		fadeSamples = nSamples / 3
	}

	dataBytes := nSamples * 4
	out := make([]byte, 44+dataBytes)
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+dataBytes))
	copy(out[8:16], "WAVEfmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16)
	binary.LittleEndian.PutUint16(out[20:22], 1)
	binary.LittleEndian.PutUint16(out[22:24], 2)
	binary.LittleEndian.PutUint32(out[24:28], sr)
	binary.LittleEndian.PutUint32(out[28:32], sr*4)
	binary.LittleEndian.PutUint16(out[32:34], 4)
	binary.LittleEndian.PutUint16(out[34:36], 16)
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(dataBytes))

	dst := out[44:]
	var mLpL, mLpR float64
	for i := 0; i < nSamples; i++ {
		mLpL += masterCut * (bufL[i] - mLpL)
		mLpR += masterCut * (bufR[i] - mLpR)
		env := 1.0
		if i < fadeSamples {
			env = float64(i) / float64(fadeSamples)
		} else if i > nSamples-fadeSamples {
			env = float64(nSamples-i) / float64(fadeSamples)
		}
		l := math.Tanh(mLpL*1.32) * 0.86 * env
		r := math.Tanh(mLpR*1.32) * 0.86 * env
		binary.LittleEndian.PutUint16(dst[i*4:i*4+2], uint16(int16(l*32767.0)))
		binary.LittleEndian.PutUint16(dst[i*4+2:i*4+4], uint16(int16(r*32767.0)))
	}
	return out
}
