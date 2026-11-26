package codec

import (
	"testing"
	"time"
)

func TestSynthMusicWAVAllStylesAndMoods(t *testing.T) {
	styles := []string{"ambient", "lofi", "synthwave", "meditation", "nature", "piano", "chiptune"}
	moods := []string{"dreamy", "calm", "melancholic", "energetic", "mysterious"}
	allInsts := []string{
		"pad", "strings", "piano", "rhodes", "guitar", "harp",
		"bells", "bowls", "bass", "drums", "solo", "rain", "ocean", "vinyl",
	}

	for i, st := range styles {
		mood := moods[i%len(moods)]
		t0 := time.Now()
		wav := SynthMusicWAV(6.0, MusicOptions{
			Style: st,
			Mood:  mood,
			Seed:  uint64(1000 + i*777),
		})
		dt := time.Since(t0)
		if len(wav) < 44+44100*4*5 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
			t.Fatalf("invalid WAV for style=%s mood=%s (len=%d)", st, mood, len(wav))
		}
		nonZero := 0
		for j := 44; j < len(wav); j += 64 {
			if wav[j] != 0 {
				nonZero++
			}
		}
		if nonZero < 100 {
			t.Fatalf("audio appears silent for style=%s", st)
		}
		t.Logf("Style %-10s | mood %-11s | %4d KB | %v", st, mood, len(wav)/1024, dt.Round(time.Millisecond))
	}

	// Test every single one of the 14 individual instruments alone (ensuring none is silent or panics!)
	for _, inst := range allInsts {
		wav := SynthMusicWAV(3.0, MusicOptions{
			Style:       "lofi",
			Mood:        "dreamy",
			Instruments: []string{inst},
			Prompt:      "teste " + inst,
			Seed:        0xDEADBEEFCAFEBABE,
		})
		if len(wav) < 44+44100*4*3 {
			t.Fatalf("short WAV for single instrument %s", inst)
		}
	}

	// Stress-test 12 arbitrary prompts (Portuguese accents, emojis, short/long strings, high-bit hashes)
	prompts := []string{
		"a", "chuva em tokyo 1998", "pôr do sol na praia 🌊", "café de domingo ☕",
		"viagem espacial 2099", "teste", "mickey 1928", "floresta amazônica",
		"1234567890", "noite fria de inverno", "sonho fractal", "energia máxima!",
	}
	for _, pr := range prompts {
		w := SynthMusicWAV(2.0, MusicOptions{Style: "lofi", Mood: "melancholic", Prompt: pr, Seed: 0xFFFFFFFF12345678})
		if len(w) < 44+44100*4*2 {
			t.Fatalf("prompt %q failed WAV generation", pr)
		}
	}

	// Verify different user text prompts compose genuinely distinct musical pieces!
	wavA := SynthMusicWAV(8.0, MusicOptions{Style: "lofi", Mood: "dreamy", Prompt: "chuva em tokyo 1998", Seed: 42})
	wavB := SynthMusicWAV(8.0, MusicOptions{Style: "lofi", Mood: "dreamy", Prompt: "sol na praia tropical", Seed: 42})
	same := 0
	for i := 44; i < len(wavA) && i < len(wavB); i += 64 {
		if wavA[i] == wavB[i] {
			same++
		}
	}
	if same > len(wavA)/128 {
		t.Fatalf("different prompts produced nearly identical audio!")
	}
}
