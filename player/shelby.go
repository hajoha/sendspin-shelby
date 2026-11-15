// ABOUTME: Output backend for the "snd_shelby" PCM device (Bose snd-shelby2 driver)
// ABOUTME: Writes 48 kHz stereo s32le in exact 4096-byte blocks, no ALSA involved
package output

// Facts about /dev/snd/snd_shelby (Bose snd-shelby2.ko, kernel 3.14), all
// verified on the speaker:
//
//   - TX DMA to McASP0 runs from the module's probe at boot and plays silence
//     while nobody writes.
//   - The device MUST be opened O_RDWR. An O_WRONLY open runs the driver's
//     TX reset path (edma_stop) and the DMA never comes back until reboot
//     ("tx" in /proc/snd_shelby is ignored after probe).
//   - Every write() is copied into one 4096-byte DMA buffer WITHOUT a length
//     check: 512 frames x 2 channels x 32 bit. Writes must be exactly 4096
//     bytes. A write blocks until the DMA has freed a buffer, so the hardware
//     clock (48 kHz) paces the caller.
//   - Samples are s32le, left-justified (24-bit value << 8).
//   - The amp must be powered (gpio75); this backend only toggles mute_out
//     (gpio72). The codec must already be configured by the start script.
//
// Two volume modes (SENDSPIN_SHELBY_MODE):
//
//   - "dsp" (default): codec runs Bose's rhino_sm2_normal.cfg (Bose's sound
//     tuning). That config loads with the volume coefficient at 0 = silence;
//     the volume lives ONLY in 8 miniDSP coefficients at page 44, regs 16..47
//     (= offset 44*256+16 = 11280 of /dev/snd_aic3256), exactly as Bose's
//     APServer does it (captured on the stock firmware):
//       C0 = 0.5 * 10^(dB/20), C1 = dB/128 (24-bit Q1.23, big-endian, 4th
//       byte 0), C2..C7 constants. Written to both adaptive buffers:
//       lseek(11280) + write 32 bytes, lseek(0, SEEK_END) = driver flips the
//       buffers, lseek(11280) + write again. Samples go out unscaled.
//   - "passthrough": codec runs rhino_sm2_passthrough.cfg (no Bose tuning,
//     fixed DSP level); volume is applied in software with a level cap.

import (
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	shelbyRate       = 48000
	shelbyChannels   = 2
	shelbyFrames     = 512
	shelbyBlockBytes = shelbyFrames * shelbyChannels * 4 // 4096
	shelbyIdleMute   = 3 * time.Second

	// SENDSPIN_SHELBY_MAX_DB = level at volume 100, meaning depends on mode.
	// passthrough: software cap below full scale (full scale is far too loud).
	shelbyDefaultMaxDB = -24.0
	// dsp: Bose volume coefficient in dB. Bose's own passthrough config pins
	// it at -40 dB; the stock capture listened at about -57 dB.
	shelbyDefaultDSPMaxDB = -40.0
	shelbyDSPCeilingDB    = -20.0 // never allow more than this
	shelbyDSPMinDB        = -127.0

	shelbyCodecDev    = "/dev/snd_aic3256"
	shelbyCoeffOffset = 44*256 + 16 // page 44 (DAC coefficient buffer), reg 16
)

// Coefficients C2..C7 exactly as APServer writes them with every volume change.
var shelbyDSPConst = [6]int32{0, 0, 0x400000, 0x32d645, 0x32d645, 0}

// Shelby implements Output for the SoundTouch snd_shelby character device.
type Shelby struct {
	devPath  string
	mutePath string // e.g. /sys/class/gpio/gpio72/value ("1" = amp unmuted); empty = don't touch
	dspMode  bool
	maxDB    float64
	maxGain  float64 // passthrough: linear gain at volume 100

	codecMu sync.Mutex // guards codec, lastDSP
	codec   *os.File
	lastDSP []byte

	mu        sync.Mutex // guards volume, muted, ampOn, lastWrite
	volume    int
	muted     bool
	ampOn     bool
	lastWrite time.Time

	f        *os.File
	inRate   int
	inCh     int
	block    []byte
	fill     int
	res      *linearResampler
	frameBuf []int32
	stopIdle chan struct{}
}

// NewShelby returns an Output writing to devPath (normally /dev/snd/snd_shelby).
// mutePath is the sysfs value file of the amp's mute_out GPIO, or "".
func NewShelby(devPath, mutePath string) Output {
	dspMode := os.Getenv("SENDSPIN_SHELBY_MODE") != "passthrough"
	maxDB, ceiling := shelbyDefaultMaxDB, 0.0
	if dspMode {
		maxDB, ceiling = shelbyDefaultDSPMaxDB, shelbyDSPCeilingDB
	}
	if v := os.Getenv("SENDSPIN_SHELBY_MAX_DB"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f <= ceiling {
			maxDB = f
		} else {
			log.Printf("snd_shelby: ignoring SENDSPIN_SHELBY_MAX_DB=%q (want a number <= %.0f)", v, ceiling)
		}
	}
	s := &Shelby{
		devPath:  devPath,
		mutePath: mutePath,
		dspMode:  dspMode,
		maxDB:    maxDB,
		maxGain:  1,
		volume:   100,
	}
	if !dspMode {
		s.maxGain = math.Pow(10, maxDB/20)
	}
	return s
}

func (s *Shelby) Open(sampleRate, channels, bitDepth int) error {
	if channels != 1 && channels != 2 {
		return fmt.Errorf("snd_shelby: unsupported channel count %d", channels)
	}
	if s.f == nil {
		// O_RDWR on purpose, see the package comment above. Never O_WRONLY.
		f, err := os.OpenFile(s.devPath, os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("snd_shelby: open %s: %w", s.devPath, err)
		}
		s.f = f
		s.block = make([]byte, shelbyBlockBytes)
		s.stopIdle = make(chan struct{})
		go s.idleMuter(s.stopIdle)
	}
	s.inRate, s.inCh = sampleRate, channels
	s.res = nil
	if sampleRate != shelbyRate {
		s.res = newLinearResampler(sampleRate, shelbyRate)
	}
	mode := "passthrough (software volume)"
	if s.dspMode {
		mode = "dsp (Bose normal.cfg volume coefficients)"
	}
	log.Printf("snd_shelby: stream %d Hz / %d ch / %d bit -> 48000 Hz / 2 ch / s32le (resample: %v, mode %s, volume 100 = %.1f dB)",
		sampleRate, channels, bitDepth, s.res != nil, mode, s.maxDB)
	s.applyDSPVolume()
	return nil
}

// Write takes interleaved samples in the int32 24-bit range.
func (s *Shelby) Write(samples []int32) error {
	if s.f == nil {
		return fmt.Errorf("snd_shelby: output not open")
	}
	s.mu.Lock()
	vol, muted := s.volume, s.muted
	s.lastWrite = time.Now()
	needAmp := !s.ampOn
	s.mu.Unlock()
	if needAmp {
		s.setAmp(true)
	}

	// passthrough: Sendspin's perceptual volume curve times the level cap
	// (<= 1, so a 24-bit sample can never leave the 24-bit range).
	// dsp: samples go out unscaled, the miniDSP applies the volume.
	gain := 1.0
	if !s.dspMode {
		gain = getVolumeMultiplier(vol, muted) * s.maxGain
	} else if muted {
		gain = 0
	}

	// To stereo frames (frameBuf is reused; the resampler has its own buffer).
	stereo := s.frameBuf[:0]
	if s.inCh == 1 {
		for _, v := range samples {
			g := int32(float64(v) * gain)
			stereo = append(stereo, g, g)
		}
	} else {
		for _, v := range samples {
			stereo = append(stereo, int32(float64(v)*gain))
		}
	}
	s.frameBuf = stereo[:0]
	frames := stereo
	if s.res != nil {
		frames = s.res.process(stereo)
	}

	for _, v := range frames {
		u := uint32(v << 8) // 24-bit -> left-justified 32-bit
		b := s.block[s.fill : s.fill+4]
		b[0], b[1], b[2], b[3] = byte(u), byte(u>>8), byte(u>>16), byte(u>>24)
		s.fill += 4
		if s.fill == shelbyBlockBytes {
			n, err := s.f.Write(s.block)
			s.fill = 0
			if err != nil {
				return fmt.Errorf("snd_shelby: write: %w", err)
			}
			if n != shelbyBlockBytes {
				return fmt.Errorf("snd_shelby: short write %d of %d", n, shelbyBlockBytes)
			}
		}
	}
	return nil
}

func (s *Shelby) Close() error {
	if s.f == nil {
		return nil
	}
	close(s.stopIdle)
	s.setAmp(false)
	err := s.f.Close()
	s.f = nil
	s.fill = 0
	s.codecMu.Lock()
	if s.codec != nil {
		s.codec.Close()
		s.codec = nil
		s.lastDSP = nil
	}
	s.codecMu.Unlock()
	return err
}

func (s *Shelby) SetVolume(volume int) {
	if volume < 0 {
		volume = 0
	} else if volume > 100 {
		volume = 100
	}
	s.mu.Lock()
	s.volume = volume
	s.mu.Unlock()
	s.applyDSPVolume()
}

func (s *Shelby) SetMuted(muted bool) {
	s.mu.Lock()
	s.muted = muted
	s.mu.Unlock()
	s.applyDSPVolume()
}

// dspVolumeDB maps a 0..100 volume onto the Bose volume coefficient: the
// Sendspin perceptual curve (vol/100)^1.5 in dB below maxDB. 0 or muted
// gives the minimum.
func dspVolumeDB(volume int, muted bool, maxDB float64) float64 {
	if muted || volume <= 0 {
		return shelbyDSPMinDB
	}
	db := maxDB + 30*math.Log10(float64(volume)/100)
	if db < shelbyDSPMinDB {
		db = shelbyDSPMinDB
	}
	return db
}

// dspVolumeBlock builds the 32-byte coefficient block APServer writes.
func dspVolumeBlock(db float64, silent bool) []byte {
	c := make([]int32, 0, 8)
	if silent {
		c = append(c, 0, -0x800000) // gain 0, -128 dB
	} else {
		c0 := math.Round(0.5 * math.Pow(10, db/20) * (1 << 23))
		c1 := math.Round(db / 128 * (1 << 23))
		c = append(c, int32(math.Min(c0, 0x7fffff)), int32(math.Max(c1, -0x800000)))
	}
	c = append(c, shelbyDSPConst[:]...)
	b := make([]byte, 32)
	for i, v := range c {
		u := uint32(v) & 0xffffff
		b[4*i], b[4*i+1], b[4*i+2], b[4*i+3] = byte(u>>16), byte(u>>8), byte(u), 0
	}
	return b
}

// applyDSPVolume writes the current volume into both adaptive coefficient
// buffers of the codec, like APServer: write, flip (SEEK_END), write.
func (s *Shelby) applyDSPVolume() {
	if !s.dspMode {
		return
	}
	s.mu.Lock()
	vol, muted := s.volume, s.muted
	s.mu.Unlock()
	silent := muted || vol <= 0
	db := dspVolumeDB(vol, muted, s.maxDB)
	block := dspVolumeBlock(db, silent)

	s.codecMu.Lock()
	defer s.codecMu.Unlock()
	if s.lastDSP != nil && string(s.lastDSP) == string(block) {
		return
	}
	if s.codec == nil {
		f, err := os.OpenFile(shelbyCodecDev, os.O_RDWR, 0)
		if err != nil {
			log.Printf("snd_shelby: open %s: %v", shelbyCodecDev, err)
			return
		}
		s.codec = f
	}
	for pass := 0; pass < 2; pass++ {
		if _, err := s.codec.Seek(shelbyCoeffOffset, 0); err != nil {
			log.Printf("snd_shelby: codec seek: %v", err)
			return
		}
		if n, err := s.codec.Write(block); err != nil || n != len(block) {
			log.Printf("snd_shelby: codec write: n=%d err=%v", n, err)
			return
		}
		if pass == 0 {
			// SEEK_END makes the driver flip the miniDSP's adaptive buffers.
			if _, err := s.codec.Seek(0, 2); err != nil {
				log.Printf("snd_shelby: codec buffer flip: %v", err)
				return
			}
		}
	}
	s.lastDSP = block
	log.Printf("snd_shelby: volume %d%s -> DSP %.1f dB", vol, map[bool]string{true: " (muted)"}[muted], db)
}

// idleMuter mutes the amp when no audio has been written for a while, so
// the speaker does not hiss while idle. Write unmutes it again.
func (s *Shelby) idleMuter(stop <-chan struct{}) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.mu.Lock()
			idle := s.ampOn && time.Since(s.lastWrite) > shelbyIdleMute
			s.mu.Unlock()
			if idle {
				s.setAmp(false)
			}
		}
	}
}

func (s *Shelby) setAmp(on bool) {
	s.mu.Lock()
	s.ampOn = on
	s.mu.Unlock()
	if s.mutePath == "" {
		return
	}
	v := "0"
	if on {
		v = "1"
	}
	if err := os.WriteFile(s.mutePath, []byte(v), 0); err != nil {
		log.Printf("snd_shelby: set amp %s: %v", v, err)
	}
}

// linearResampler converts interleaved stereo int32 between sample rates.
// Unlike audio.Resampler it carries the previous frame and the fractional
// read position across calls, so no frames are lost or duplicated at chunk
// boundaries and the output duration is exact (needed for sync).
type linearResampler struct {
	step   float64 // input frames per output frame
	pos    float64 // read position; 0 = prev frame, k = k-th frame of this chunk
	prevL  int32
	prevR  int32
	output []int32
}

func newLinearResampler(inRate, outRate int) *linearResampler {
	return &linearResampler{step: float64(inRate) / float64(outRate)}
}

func (r *linearResampler) process(in []int32) []int32 {
	n := len(in) / 2
	out := r.output[:0]
	at := func(k int) (int32, int32) { // k = 0 is prev, k >= 1 is in[k-1]
		if k == 0 {
			return r.prevL, r.prevR
		}
		return in[2*(k-1)], in[2*(k-1)+1]
	}
	for r.pos < float64(n) {
		k := int(r.pos)
		frac := r.pos - float64(k)
		l0, r0 := at(k)
		l1, r1 := at(k + 1)
		out = append(out,
			int32(float64(l0)+(float64(l1)-float64(l0))*frac),
			int32(float64(r0)+(float64(r1)-float64(r0))*frac))
		r.pos += r.step
	}
	r.pos -= float64(n)
	if n > 0 {
		r.prevL, r.prevR = in[2*(n-1)], in[2*(n-1)+1]
	}
	r.output = out
	return out
}
