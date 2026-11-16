package output

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestShelby(t *testing.T) (*Shelby, string) {
	t.Helper()
	dev := filepath.Join(t.TempDir(), "dev")
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewShelby(dev, "").(*Shelby)
	s.procPath, s.rate = "", 48000 // no rate meter; 48 kHz in = out, no resampling
	s.fadePos = shelbyFadeFrames   // no fade-in, so samples map 1:1
	return s, dev
}

// Writes reach the device only as whole 4096-byte blocks, as s32le with the
// 24-bit sample in the upper bits.
func TestShelbyBlocksAndPacking(t *testing.T) {
	t.Setenv("SENDSPIN_SHELBY_MODE", "passthrough")
	t.Setenv("SENDSPIN_SHELBY_MAX_DB", "0")
	s, dev := newTestShelby(t)
	if err := s.Open(48000, 2, 16); err != nil {
		t.Fatal(err)
	}
	in := make([]int32, 2*1000) // 1000 frames -> one full block, rest buffered
	for i := range in {
		in[i] = int32(i) - 1000
	}
	if err := s.Write(in); err != nil {
		t.Fatal(err)
	}
	s.Close() // fades the pending tail out to a block boundary
	b, _ := os.ReadFile(dev)
	if len(b) == 0 || len(b)%shelbyBlockBytes != 0 {
		t.Fatalf("want whole %d-byte blocks, got %d bytes", shelbyBlockBytes, len(b))
	}
	for i := 0; i < shelbyBlockBytes/4; i++ {
		got := int32(binary.LittleEndian.Uint32(b[4*i:]))
		if want := (int32(i) - 1000) << 8; got != want {
			t.Fatalf("sample %d: got %d want %d", i, got, want)
		}
	}
}

// In dsp mode the samples go out unscaled; the volume lives in the miniDSP.
func TestShelbyDSPModeUnscaled(t *testing.T) {
	t.Setenv("SENDSPIN_SHELBY_MODE", "dsp")
	s, dev := newTestShelby(t)
	s.Open(48000, 2, 24)
	s.SetVolume(10)
	in := make([]int32, 2*shelbyFrames)
	for i := range in {
		in[i] = 8388607
	}
	s.Write(in)
	s.Close()
	b, _ := os.ReadFile(dev)
	if got := int32(binary.LittleEndian.Uint32(b)) >> 8; got != 8388607 {
		t.Fatalf("dsp mode scaled the samples: %d", got)
	}
}

func TestShelbyLevelCap(t *testing.T) {
	t.Setenv("SENDSPIN_SHELBY_MODE", "passthrough")
	s, dev := newTestShelby(t) // default -24 dB
	s.Open(48000, 2, 16)
	in := make([]int32, 2*shelbyFrames)
	for i := range in {
		in[i] = 8388607 // 24-bit full scale
	}
	s.Write(in)
	s.Close()
	b, _ := os.ReadFile(dev)
	got := int32(binary.LittleEndian.Uint32(b)) >> 8
	const want = 529285 // 8388607 * 10^(-24/20)
	if d := got - want; d < -2 || d > 2 {
		t.Fatalf("full scale at volume 100: got %d want ~%d", got, want)
	}
}

// 10 s of input must become exactly 10 s at the DMA rate, whatever the chunking.
func TestCubicResamplerExactLength(t *testing.T) {
	for _, c := range []struct {
		in    int
		out   float64
		chunk int
	}{{44100, 48000, 441}, {48000, 46875, 1200}, {44100, 46920.5, 1102}} {
		r := newCubicResampler(c.in, c.out)
		total, inFrames := 0, 0
		for ; inFrames < 10*c.in; inFrames += c.chunk {
			total += len(r.process(make([]int32, 2*c.chunk))) / 2
		}
		if want := float64(inFrames) * c.out / float64(c.in); float64(total) < want-2 || float64(total) > want+2 {
			t.Fatalf("%d -> %.1f Hz: got %d frames, want ~%.0f", c.in, c.out, total, want)
		}
	}
}

func TestCubicResamplerContinuity(t *testing.T) {
	r := newCubicResampler(44100, 46875)
	var out []int32
	v := int32(0)
	for c := 0; c < 50; c++ {
		in := make([]int32, 2*100)
		for i := 0; i < 100; i++ {
			in[2*i], in[2*i+1] = v, v
			v += 100
		}
		out = append(out, r.process(in)...)
	}
	// A ramp stays a ramp (cubic interpolation is exact on it), with no
	// jump at the chunk boundaries. Skip the zero history at the start.
	for i := 8; i+2 < len(out); i += 2 {
		if d := out[i+2] - out[i]; d < 80 || d > 100 {
			t.Fatalf("step %d at frame %d, want ~94", d, i/2)
		}
	}
}

// Overshoot of a full-scale square wave must not wrap around.
func TestCubicResamplerClamps(t *testing.T) {
	r := newCubicResampler(44100, 46875)
	in := make([]int32, 2*400)
	for i := range in {
		if (i/40)%2 == 0 {
			in[i] = 0x7fffff
		} else {
			in[i] = -0x800000
		}
	}
	for _, v := range r.process(in) {
		if v > 0x7fffff || v < -0x800000 {
			t.Fatalf("sample %d outside 24 bit", v)
		}
	}
}

func TestReadTxDone(t *testing.T) {
	p := filepath.Join(t.TempDir(), "snd_shelby")
	proc := "mode: wifi\ngpios: rx:255 tx:255 read:255 write:255\n\ntxDoneCount: 6261782\ntxUnderflows: 5952322\n\nrxDoneCount 0\n"
	os.WriteFile(p, []byte(proc), 0o600)
	n, err := readTxDone(p)
	if err != nil || n != 6261782 {
		t.Fatalf("got %d, %v", n, err)
	}
}

// The capture from the speaker: 10997 buffers in 120 s.
func TestEstimateRate(t *testing.T) {
	r, ok := estimateRate(10997, 120*time.Second)
	if !ok || r < 46900 || r > 46940 {
		t.Fatalf("got %.1f Hz ok=%v", r, ok)
	}
	if _, ok := estimateRate(10, 120*time.Second); ok {
		t.Fatal("implausible rate accepted")
	}
}

// The coefficient block must match what the stock firmware writes. These
// (C0, C1) pairs were captured from it at three volume steps.
func TestDSPBlockMatchesStockFirmware(t *testing.T) {
	cases := []struct{ c0, c1 int32 }{
		{0x0015bf, -0x398a3e},
		{0x000e77, -0x3d147b},
		{0x001814, -0x38a7af},
	}
	consts := []byte{0, 0, 0, 0, 0, 0, 0, 0, 0x40, 0, 0, 0, 0x32, 0xd6, 0x45, 0, 0x32, 0xd6, 0x45, 0, 0, 0, 0, 0}
	for _, k := range cases {
		db := float64(k.c1) / (1 << 23) * 128
		b := dspVolumeBlock(db, false)
		c1 := int32(b[4])<<16 | int32(b[5])<<8 | int32(b[6])
		if c1&0x800000 != 0 {
			c1 -= 1 << 24
		}
		if c1 != k.c1 {
			t.Fatalf("C1 %x, stock %x", c1, k.c1)
		}
		c0 := int32(b[0])<<16 | int32(b[1])<<8 | int32(b[2])
		if r := float64(c0) / float64(k.c0); r < 0.988 || r > 1.012 { // within 0.1 dB
			t.Fatalf("C0 %x, stock %x", c0, k.c0)
		}
		for i, w := range consts {
			if b[8+i] != w {
				t.Fatalf("constant byte %d = %x, want %x", 8+i, b[8+i], w)
			}
		}
		for i := 3; i < 32; i += 4 {
			if b[i] != 0 {
				t.Fatalf("pad byte %d is not 0", i)
			}
		}
	}
	if b := dspVolumeBlock(0, true); b[0]|b[1]|b[2] != 0 || b[4] != 0x80 {
		t.Fatalf("silent block wrong: %x", b[:8])
	}
}

func TestDSPVolumeCurve(t *testing.T) {
	if db := dspVolumeDB(100, false, -40); db != -40 {
		t.Fatalf("volume 100: %v dB", db)
	}
	if db := dspVolumeDB(10, false, -40); db < -70.01 || db > -69.99 {
		t.Fatalf("volume 10: %v dB", db)
	}
	if db := dspVolumeDB(0, false, -40); db != shelbyDSPMinDB {
		t.Fatalf("volume 0: %v dB", db)
	}
	if db := dspVolumeDB(80, true, -40); db != shelbyDSPMinDB {
		t.Fatalf("muted: %v dB", db)
	}
}

func frameAt(b []byte, i int) (int32, int32) {
	return int32(binary.LittleEndian.Uint32(b[8*i:])) >> 8, int32(binary.LittleEndian.Uint32(b[8*i+4:])) >> 8
}

// A new stream ends the old one with a fade to zero right away (on a block
// boundary), drops the old stream's stragglers and fades the new one in.
func TestShelbyStreamBoundaryFades(t *testing.T) {
	t.Setenv("SENDSPIN_SHELBY_MODE", "passthrough")
	t.Setenv("SENDSPIN_SHELBY_MAX_DB", "0")
	s, dev := newTestShelby(t)
	s.Open(48000, 2, 24)
	full := func(n int) []int32 {
		in := make([]int32, 2*n)
		for i := range in {
			in[i] = 1 << 20
		}
		return in
	}
	s.Write(full(700)) // 512 written, 188 pending
	s.Open(48000, 2, 24)
	b, _ := os.ReadFile(dev)
	if len(b)%shelbyBlockBytes != 0 || len(b)/8 < 700+shelbyFadeFrames {
		t.Fatalf("%d bytes after the boundary, want the fade written out to a block boundary", len(b))
	}
	ended := len(b) / 8
	if l, _ := frameAt(b, ended-1); l != 0 {
		t.Fatalf("old stream ends at %d, want 0", l)
	}
	for i := 701; i < ended; i++ {
		prev, _ := frameAt(b, i-1)
		cur, _ := frameAt(b, i)
		if cur > prev || prev-cur > (1<<20)/shelbyFadeFrames+2 {
			t.Fatalf("fade not smooth at frame %d: %d -> %d", i, prev, cur)
		}
	}

	s.Write(full(300)) // straggler of the old stream: dropped
	s.staleEnd = time.Now()
	s.Write(full(2 * shelbyFrames)) // the new stream
	b, _ = os.ReadFile(dev)
	if l, _ := frameAt(b, ended); l != 0 {
		t.Fatalf("new stream starts at %d, want a fade-in from 0", l)
	}
	if l, _ := frameAt(b, ended+shelbyFadeFrames+5); l != 1<<20 {
		t.Fatalf("new stream after fade-in %d (straggler not dropped?)", l)
	}
	s.Close()
}

// A seek (stream/clear) without a new stream fades out the same way.
func TestShelbyStreamBoundaryOnSeek(t *testing.T) {
	t.Setenv("SENDSPIN_SHELBY_MODE", "passthrough")
	t.Setenv("SENDSPIN_SHELBY_MAX_DB", "0")
	s, dev := newTestShelby(t)
	s.Open(48000, 2, 24)
	in := make([]int32, 2*700)
	for i := range in {
		in[i] = 1 << 20
	}
	s.Write(in)
	s.StreamBoundary()
	b, _ := os.ReadFile(dev)
	if len(b)%shelbyBlockBytes != 0 || len(b)/8 < 700+shelbyFadeFrames {
		t.Fatalf("%d bytes, want the fade written out to a block boundary", len(b))
	}
	if l, _ := frameAt(b, len(b)/8-1); l != 0 {
		t.Fatalf("ends at %d, want 0", l)
	}
	if s.fadePos != 0 || s.staleEnd.IsZero() {
		t.Fatal("next audio does not fade in / stragglers not dropped")
	}
	s.Close()
}

func TestRampNext(t *testing.T) {
	const floor = -90.0
	run := func(cur float64, curSil bool, target float64, silent bool) (float64, bool, int) {
		for steps := 1; steps < 1000; steps++ {
			var done bool
			cur, curSil, done = rampNext(cur, curSil, target, silent, floor)
			if done {
				return cur, curSil, steps
			}
		}
		t.Fatal("ramp never finished")
		return 0, false, 0
	}
	if c, sil, n := run(-60, false, -50, false); c != -50 || sil || n != 20 {
		t.Fatalf("-60 -> -50: %v %v in %d steps", c, sil, n)
	}
	if c, sil, _ := run(-60, false, 0, true); c != floor || !sil {
		t.Fatalf("to silence: %v %v", c, sil)
	}
	if c, sil, n := run(floor, true, -85, false); c != -85 || sil || n != 11 {
		t.Fatalf("from silence: %v %v in %d steps", c, sil, n)
	}
	if c, _, n := run(-60, false, -110, false); c != -110 || n != 60 {
		t.Fatalf("below floor: %v in %d steps", c, n)
	}
	if c, _, n := run(-100, false, -120, false); c != -120 || n != 1 {
		t.Fatalf("both below floor: %v in %d steps", c, n)
	}
}
