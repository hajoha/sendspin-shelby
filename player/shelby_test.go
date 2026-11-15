package output

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func newTestShelby(t *testing.T) (*Shelby, string) {
	t.Helper()
	dev := filepath.Join(t.TempDir(), "dev")
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return NewShelby(dev, "").(*Shelby), dev
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
	s.Close()
	b, _ := os.ReadFile(dev)
	if len(b) != shelbyBlockBytes {
		t.Fatalf("want exactly one %d-byte block, got %d bytes", shelbyBlockBytes, len(b))
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

// 10 s of 44.1 kHz must become exactly 10 s of 48 kHz, whatever the chunking.
func TestLinearResamplerExactLength(t *testing.T) {
	r := newLinearResampler(44100, 48000)
	total := 0
	for c := 0; c < 1000; c++ {
		total += len(r.process(make([]int32, 2*441))) / 2
	}
	if total < 479999 || total > 480001 {
		t.Fatalf("got %d frames, want ~480000", total)
	}
}

func TestLinearResamplerContinuity(t *testing.T) {
	r := newLinearResampler(44100, 48000)
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
	for i := 2; i+2 < len(out); i += 2 {
		if out[i+2] < out[i] {
			t.Fatalf("ramp not monotonic across chunks at frame %d", i/2)
		}
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
