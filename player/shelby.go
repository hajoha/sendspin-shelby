// ABOUTME: Output backend for the "snd_shelby" PCM device (Bose snd-shelby2 driver)
// ABOUTME: Writes stereo s32le in exact 4096-byte blocks at the measured DMA rate, no ALSA involved
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
//     bytes.
//   - There is no queue, only two DMA buffers (ping/pong; from the
//     disassembled module). write() waits for the next buffer-done
//     interrupt, copies into the buffer that is not playing and returns.
//     The interrupt counts txDoneCount; if no write came since the previous
//     interrupt it counts txUnderflows and zeroes that buffer. So after each
//     write the next block must arrive within ONE block period (10.9 ms) or
//     it is an underflow (an audible click). The backend therefore writes
//     from a dedicated thread at real-time priority (a native C thread, outside the Go runtime), fed by
//     a small block ring.
//   - That clock is NOT 48 kHz: the DMA completes about 91.6 buffers/s, i.e.
//     about 46.9 kHz (12 MHz / 256 = 46875 Hz fits). Fed 48 kHz, the backlog
//     grows ~22 ms per second and sendspin-go drops a late chunk about once a
//     second (an audible click). So the backend measures the real rate from
//     txDoneCount in /proc/snd_shelby and resamples every stream to it.
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

/*
#cgo LDFLAGS: -lpthread
#include <errno.h>
#include <pthread.h>
#include <sched.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

// The device writer, outside the Go runtime: a native thread at SCHED_FIFO
// that takes full blocks from a ring and write()s them. Go threads need a
// Go scheduler slot (P) after every syscall; on this single-core speaker
// that wait was measured at up to 1.9 s (GC), and the driver allows 10.9 ms.
enum { SHELBY_BLOCK = 4096 };

typedef struct {
	int fd, nblocks, prio, rt_err;
	unsigned char *buf;
	unsigned long head, tail;  // blocks pushed / written; guarded by mu
	int stop, err;             // err: errno of the first failed write
	unsigned long starved, wrote;
	long write_max_ns;
	pthread_mutex_t mu;
	pthread_cond_t cv;
	pthread_t th;
} shelby_ring;

static long shelby_now_ns(void) {
	struct timespec t;
	clock_gettime(CLOCK_MONOTONIC, &t);
	return t.tv_sec * 1000000000L + t.tv_nsec;
}

static void *shelby_writer(void *arg) {
	shelby_ring *r = arg;
	if (r->prio > 0) {
		struct sched_param sp = { .sched_priority = r->prio };
		r->rt_err = pthread_setschedparam(pthread_self(), SCHED_FIFO, &sp);
	}
	pthread_mutex_lock(&r->mu);
	for (;;) {
		if (r->head == r->tail && !r->stop) {
			r->starved++;
			while (r->head == r->tail && !r->stop)
				pthread_cond_wait(&r->cv, &r->mu);
		}
		if (r->head == r->tail)
			break; // stopped and drained
		unsigned char *b = r->buf + (r->tail % r->nblocks) * SHELBY_BLOCK;
		int failed = r->err != 0;
		pthread_mutex_unlock(&r->mu);
		if (!failed) {
			long t0 = shelby_now_ns();
			ssize_t n;
			do {
				n = write(r->fd, b, SHELBY_BLOCK);
			} while (n < 0 && errno == EINTR);
			long d = shelby_now_ns() - t0;
			pthread_mutex_lock(&r->mu);
			// Never retry a failing device at real-time priority: stop
			// writing, keep draining, report it.
			if (n != SHELBY_BLOCK)
				r->err = n < 0 ? errno : EIO;
			r->wrote++;
			if (d > r->write_max_ns)
				r->write_max_ns = d;
		} else {
			pthread_mutex_lock(&r->mu);
		}
		r->tail++;
		pthread_cond_broadcast(&r->cv);
	}
	pthread_mutex_unlock(&r->mu);
	return NULL;
}

static shelby_ring *shelby_ring_start(int fd, int nblocks, int prio) {
	shelby_ring *r = calloc(1, sizeof *r);
	if (!r)
		return NULL;
	r->buf = malloc((size_t)nblocks * SHELBY_BLOCK);
	if (!r->buf) {
		free(r);
		return NULL;
	}
	r->fd = fd;
	r->nblocks = nblocks;
	r->prio = prio;
	pthread_mutex_init(&r->mu, NULL);
	pthread_cond_init(&r->cv, NULL);
	if (pthread_create(&r->th, NULL, shelby_writer, r) != 0) {
		free(r->buf);
		free(r);
		return NULL;
	}
	return r;
}

// Copies one block in, waiting while the ring is full. Returns the write
// errno so far (0 = fine).
static int shelby_ring_push(shelby_ring *r, const unsigned char *block) {
	pthread_mutex_lock(&r->mu);
	while (r->head - r->tail == (unsigned long)r->nblocks && !r->err)
		pthread_cond_wait(&r->cv, &r->mu);
	int err = r->err;
	unsigned long slot = r->head % r->nblocks;
	pthread_mutex_unlock(&r->mu);
	if (err)
		return err;
	// The writer never touches an unpublished slot: copy without the lock.
	memcpy(r->buf + slot * SHELBY_BLOCK, block, SHELBY_BLOCK);
	pthread_mutex_lock(&r->mu);
	r->head++;
	pthread_cond_broadcast(&r->cv);
	pthread_mutex_unlock(&r->mu);
	return 0;
}

// Waits until every pushed block has been written (or the device failed).
static void shelby_ring_drain(shelby_ring *r) {
	pthread_mutex_lock(&r->mu);
	while (r->tail != r->head)
		pthread_cond_wait(&r->cv, &r->mu);
	pthread_mutex_unlock(&r->mu);
}

typedef struct {
	unsigned long starved, wrote, fill;
	long write_max_ns;
	int err, rt_err;
} shelby_stats;

static shelby_stats shelby_ring_stats(shelby_ring *r, int reset) {
	shelby_stats st;
	pthread_mutex_lock(&r->mu);
	st.starved = r->starved;
	st.wrote = r->wrote;
	st.fill = r->head - r->tail;
	st.write_max_ns = r->write_max_ns;
	st.err = r->err;
	st.rt_err = r->rt_err;
	if (reset) {
		r->starved = r->wrote = 0;
		r->write_max_ns = 0;
	}
	pthread_mutex_unlock(&r->mu);
	return st;
}

// Stops the writer after it has written what is queued, and frees the ring.
static void shelby_ring_stop(shelby_ring *r) {
	pthread_mutex_lock(&r->mu);
	r->stop = 1;
	pthread_cond_broadcast(&r->cv);
	pthread_mutex_unlock(&r->mu);
	pthread_join(r->th, NULL);
	pthread_mutex_destroy(&r->mu);
	pthread_cond_destroy(&r->cv);
	free(r->buf);
	free(r);
}
*/
import "C"

import (
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	// Initial guess for the DMA rate until the meter has measured it.
	shelbyNominalRate = 46875.0
	shelbyProcPath    = "/proc/snd_shelby"
	// The rate estimate is (buffers counted) / (time) since the meter started;
	// it is used once this much time has passed (error about 1024 Hz / window).
	shelbyRateMinWindow = 20 * time.Second
	shelbyRateMin       = 40000.0 // sanity range for a measured rate
	shelbyRateMax       = 52000.0

	shelbyChannels   = 2
	shelbyFrames     = 512
	shelbyBlockBytes = shelbyFrames * shelbyChannels * 4 // 4096
	shelbyIdleMute   = 3 * time.Second

	// Stream edges. At a new stream (track change, resume) or a stream/clear
	// (seek) sendspin-go cuts the audio off: its remaining audio is discarded and ~1 s of silence
	// follows while the new audio buffers. To avoid the hard step, the backend fades
	// the last frame to zero (at least shelbyFadeFrames, up to
	// the next block boundary) and writes it out at once; stragglers of the
	// old stream arriving within shelbyStaleWindow are dropped; the new
	// stream fades in.
	//
	// Adding frames is fine ONLY there, because silence follows anyway.
	// Mid-stream, never add frames: playout is paced by the DMA, so extra
	// audio delays everything behind it, sendspin-go drops chunks that are
	// >50 ms late, and the gaps cascade (tried: a 45 ms lead-in after every
	// gap gave ~700 drops per minute; one per stream changed nothing).
	shelbyFadeFrames  = 256 // ~5.5 ms
	shelbyStaleWindow = 200 * time.Millisecond

	// Blocks between the packing code and the writer thread (87 ms). Enough
	// for the jitter of sendspin-go's hand-over (gaps up to ~80 ms seen);
	// more only adds latency once it fills after a stall.
	shelbyRingBlocks = 8
	// SCHED_FIFO priority of the writer thread (SENDSPIN_SHELBY_RT_PRIO,
	// 0 = normal scheduling).
	shelbyDefaultRTPrio = 20

	shelbyStatsEvery = time.Minute

	// DSP volume changes ramp in steps instead of jumping (zipper/click).
	shelbyRampStepDB   = 0.5
	shelbyRampInterval = 2 * time.Millisecond
	shelbyRampRangeDB  = 50.0 // ramp from/to maxDB-50; below that, jump

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

	codecMu sync.Mutex // guards codec
	codec   *os.File

	rampMu     sync.Mutex // guards dspTarget, dspSilent, dspCur, dspCurSilent
	dspTarget  float64
	dspSilent  bool
	dspCur     float64 // what the codec holds now
	dspCurSil  bool
	rampKick   chan struct{}
	rampActive bool

	mu        sync.Mutex // guards volume, muted, ampOn, lastWrite
	volume    int
	muted     bool
	ampOn     bool
	lastWrite time.Time

	procPath string     // txDoneCount source; empty = never measure
	rateMu   sync.Mutex // guards rate
	rate     float64    // DMA rate in Hz (measured, or the nominal guess)

	// writeMu serialises everything that touches the device stream: Write,
	// Open/Close at stream boundaries and the edge flusher.
	writeMu  sync.Mutex
	f        *os.File
	inRate   int
	inCh     int
	block    []byte
	fill     int
	res      *cubicResampler
	frameBuf []int32
	stopIdle chan struct{}
	fadePos  int           // fade-in progress in frames (>= shelbyFadeFrames: done)
	curGain  float64       // passthrough: smoothed software gain
	lastL    int32         // last frame put into block
	lastR    int32         //
	staleEnd time.Time     // drop writes until then (old stream after a boundary)
	stale    int           // frames dropped in the current window (for the log)
	gapMax   time.Duration // longest time between two Write calls (stats)
	lastCall time.Time

	// The native writer thread and its block ring (see the C preamble).
	ring   *C.shelby_ring
	rtPrio int
}

// dmaCount holds the driver's counters: txDoneCount counts every DMA buffer
// played, txUnderflows those that were silence because no write came in time.
type dmaCount struct{ done, under int64 }

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
		procPath: shelbyProcPath,
		rate:     shelbyNominalRate,
		fadePos:  0, // the first stream fades in
		rtPrio:   rtPrioFromEnv(),
		curGain:  -1, // unset: start at the target gain
		// normal.cfg loads with the volume coefficient at 0 (silent).
		dspCur:    maxDB - shelbyRampRangeDB,
		dspCurSil: true,
		rampKick:  make(chan struct{}, 1),
	}
	if !dspMode {
		s.maxGain = math.Pow(10, maxDB/20)
	} else {
		go s.dspRamper()
	}
	return s
}

func (s *Shelby) Open(sampleRate, channels, bitDepth int) error {
	if channels != 1 && channels != 2 {
		return fmt.Errorf("snd_shelby: unsupported channel count %d", channels)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.f == nil {
		// O_RDWR on purpose, see the package comment above. Never O_WRONLY.
		f, err := os.OpenFile(s.devPath, os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("snd_shelby: open %s: %w", s.devPath, err)
		}
		s.f = f
		s.block = make([]byte, shelbyBlockBytes)
		s.ring = C.shelby_ring_start(C.int(f.Fd()), shelbyRingBlocks, C.int(s.rtPrio))
		if s.ring == nil {
			f.Close()
			s.f = nil
			return fmt.Errorf("snd_shelby: cannot start the writer thread")
		}
		go s.logWriterPrio()
		s.stopIdle = make(chan struct{})
		go s.idleMuter(s.stopIdle)
		if s.procPath != "" {
			go s.rateMeter(s.stopIdle)
			go s.dmaStats(s.stopIdle, s.ring)
		}
	} else {
		s.boundaryLocked() // a new stream
	}
	s.inRate, s.inCh = sampleRate, channels
	s.res = nil
	rate := s.dmaRate()
	if float64(sampleRate) != rate {
		s.res = newCubicResampler(sampleRate, rate)
	}
	mode := "passthrough (software volume)"
	if s.dspMode {
		mode = "dsp (Bose normal.cfg volume coefficients)"
	}
	log.Printf("snd_shelby: stream %d Hz / %d ch / %d bit -> %.1f Hz DMA / 2 ch / s32le (resample: %v, mode %s, volume 100 = %.1f dB)",
		sampleRate, channels, bitDepth, rate, s.res != nil, mode, s.maxDB)
	s.applyDSPVolume()
	return nil
}

// Write takes interleaved samples in the int32 24-bit range.
func (s *Shelby) Write(samples []int32) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.f == nil {
		return fmt.Errorf("snd_shelby: output not open")
	}
	if now := time.Now(); !s.lastCall.IsZero() {
		if g := now.Sub(s.lastCall); g > s.gapMax {
			s.gapMax = g
		}
		s.lastCall = now
	} else {
		s.lastCall = now
	}
	if !s.staleEnd.IsZero() {
		if time.Now().Before(s.staleEnd) {
			s.stale += len(samples) / s.inCh
			return nil
		}
		if s.stale > 0 {
			log.Printf("snd_shelby: dropped %d frames of the previous stream", s.stale)
		}
		s.staleEnd = time.Time{}
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
	// (<= 1, so a 24-bit sample can never leave the 24-bit range), smoothed
	// per frame so volume changes do not step. dsp: samples go out
	// unscaled, the miniDSP applies (and ramps) the volume.
	target := 1.0
	if !s.dspMode {
		target = getVolumeMultiplier(vol, muted) * s.maxGain
	}
	if s.curGain < 0 {
		s.curGain = target
	}

	// To stereo frames (frameBuf is reused; the resampler has its own buffer).
	stereo := s.frameBuf[:0]
	step := s.inCh
	for i := 0; i+step <= len(samples); i += step {
		if s.curGain != target {
			s.curGain += (target - s.curGain) / 480 // ~10 ms time constant
			if math.Abs(target-s.curGain) < 1e-6 {
				s.curGain = target
			}
		}
		l := int32(float64(samples[i]) * s.curGain)
		r := l
		if step == 2 {
			r = int32(float64(samples[i+1]) * s.curGain)
		}
		stereo = append(stereo, l, r)
	}
	s.frameBuf = stereo[:0]
	frames := stereo
	if rate := s.dmaRate(); s.res != nil || float64(s.inRate) != rate {
		if s.res == nil {
			s.res = newCubicResampler(s.inRate, rate)
		}
		s.res.setRates(s.inRate, rate)
		frames = s.res.process(stereo)
	}
	if len(frames) == 0 {
		return nil
	}

	for i := 0; s.fadePos < shelbyFadeFrames && i+1 < len(frames); i += 2 {
		g := float64(s.fadePos) / shelbyFadeFrames
		frames[i] = int32(float64(frames[i]) * g)
		frames[i+1] = int32(float64(frames[i+1]) * g)
		s.fadePos++
	}
	return s.packLocked(frames)
}

// packLocked appends stereo frames to the block and hands every full block
// to the writer thread (blocking while the ring is full). The caller holds
// writeMu.
func (s *Shelby) packLocked(frames []int32) error {
	for _, v := range frames {
		u := uint32(v << 8) // 24-bit -> left-justified 32-bit
		b := s.block[s.fill : s.fill+4]
		b[0], b[1], b[2], b[3] = byte(u), byte(u>>8), byte(u>>16), byte(u>>24)
		s.fill += 4
		if s.fill == shelbyBlockBytes {
			s.fill = 0
			if e := C.shelby_ring_push(s.ring, (*C.uchar)(unsafe.Pointer(&s.block[0]))); e != 0 {
				return fmt.Errorf("snd_shelby: write: %w", syscall.Errno(e))
			}
		}
	}
	if n := len(frames); n >= 2 {
		s.lastL, s.lastR = frames[n-2], frames[n-1]
	}
	return nil
}

// StreamBoundary is called (via the main patch) when sendspin-go has just
// cut the audio off without a new stream, i.e. a stream/clear on seek.
func (s *Shelby) StreamBoundary() {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.f != nil {
		s.boundaryLocked()
	}
}

// boundaryLocked ends the current audio with a fade, ignores stragglers of
// it for shelbyStaleWindow and makes the next audio fade in. The caller
// holds writeMu.
func (s *Shelby) boundaryLocked() {
	if err := s.fadeOutLocked(); err != nil {
		log.Printf("snd_shelby: %v", err)
	}
	s.staleEnd, s.stale = time.Now().Add(shelbyStaleWindow), 0
	s.fadePos = 0
	s.lastCall = time.Time{} // the gap before the next stream is not jitter
}

// logWriterPrio reports once whether the writer got real-time priority.
func (s *Shelby) logWriterPrio() {
	time.Sleep(100 * time.Millisecond)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.ring == nil || s.rtPrio == 0 {
		return
	}
	if st := C.shelby_ring_stats(s.ring, 0); st.rt_err != 0 {
		log.Printf("snd_shelby: writer: SCHED_FIFO %d: %v (normal priority)", s.rtPrio, syscall.Errno(st.rt_err))
	} else {
		log.Printf("snd_shelby: native writer thread at SCHED_FIFO %d", s.rtPrio)
	}
}

// drain waits until the writer has written every block handed over so far.
func (s *Shelby) drain() {
	s.writeMu.Lock()
	r := s.ring
	s.writeMu.Unlock()
	if r != nil {
		C.shelby_ring_drain(r)
	}
}

func rtPrioFromEnv() int {
	v := os.Getenv("SENDSPIN_SHELBY_RT_PRIO")
	if v == "" {
		return shelbyDefaultRTPrio
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 || n > 99 {
		log.Printf("snd_shelby: ignoring SENDSPIN_SHELBY_RT_PRIO=%q (want 0..99)", v)
		return shelbyDefaultRTPrio
	}
	return n
}

// fadeOutLocked ramps the last frame down to zero over at least
// shelbyFadeFrames, up to a block boundary, and writes it, so the output
// ends at silence instead of stepping there. It ADDS frames: use it only
// where silence follows (stream boundary, close). The caller holds writeMu.
func (s *Shelby) fadeOutLocked() error {
	if s.f == nil {
		return nil
	}
	n := shelbyFrames - s.fill/8
	if n < shelbyFadeFrames {
		n += shelbyFrames
	}
	ramp := make([]int32, 0, 2*n)
	for i := 1; i <= n; i++ {
		g := 1 - float64(i)/float64(n)
		ramp = append(ramp, int32(float64(s.lastL)*g), int32(float64(s.lastR)*g))
	}
	return s.packLocked(ramp)
}

func (s *Shelby) Close() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.f == nil {
		return nil
	}
	if err := s.fadeOutLocked(); err != nil {
		log.Printf("snd_shelby: %v", err)
	}
	close(s.stopIdle)
	C.shelby_ring_stop(s.ring)
	s.ring = nil
	s.setAmp(false)
	err := s.f.Close()
	s.f = nil
	s.fill = 0
	s.fadePos = 0
	s.codecMu.Lock()
	if s.codec != nil {
		s.codec.Close()
		s.codec = nil
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

// applyDSPVolume sets the ramp target from the current volume/mute state;
// dspRamper moves the codec there step by step.
func (s *Shelby) applyDSPVolume() {
	if !s.dspMode {
		return
	}
	s.mu.Lock()
	vol, muted := s.volume, s.muted
	s.mu.Unlock()
	silent := muted || vol <= 0
	db := dspVolumeDB(vol, muted, s.maxDB)

	s.rampMu.Lock()
	changed := s.dspTarget != db || s.dspSilent != silent
	s.dspTarget, s.dspSilent = db, silent
	s.rampMu.Unlock()
	if changed {
		log.Printf("snd_shelby: volume %d%s -> DSP %.1f dB", vol, map[bool]string{true: " (muted)"}[muted], db)
	}
	select {
	case s.rampKick <- struct{}{}:
	default:
	}
}

// rampNext returns the next coefficient state on the way from (cur,
// curSilent) to (target, silent), and whether that is the end state. Changes
// above floor move in shelbyRampStepDB steps; below floor they jump (too
// quiet to click). Silence is entered and left at the floor.
func rampNext(cur float64, curSilent bool, target float64, silent bool, floor float64) (float64, bool, bool) {
	goal := target
	if silent {
		goal = floor
	}
	if curSilent {
		if silent {
			return cur, true, true
		}
		if goal <= floor {
			return goal, false, true
		}
		return floor, false, false
	}
	if cur <= floor && goal <= floor {
		cur = goal
	} else {
		rt := math.Max(goal, floor) // ramp target, never below the floor
		switch {
		case cur < floor:
			cur = floor
		case math.Abs(rt-cur) <= shelbyRampStepDB:
			cur = rt
		case rt > cur:
			cur += shelbyRampStepDB
		default:
			cur -= shelbyRampStepDB
		}
		if cur == rt && goal < floor {
			cur = goal
		}
	}
	if cur != goal {
		return cur, false, false
	}
	return cur, silent, true
}

func (s *Shelby) dspRamper() {
	floor := s.maxDB - shelbyRampRangeDB
	for range s.rampKick {
		for {
			s.rampMu.Lock()
			cur, curSil, target, silent := s.dspCur, s.dspCurSil, s.dspTarget, s.dspSilent
			s.rampMu.Unlock()
			next, nextSil, done := rampNext(cur, curSil, target, silent, floor)
			if next != cur || nextSil != curSil {
				if !s.writeDSP(dspVolumeBlock(next, nextSil)) {
					break
				}
				s.rampMu.Lock()
				s.dspCur, s.dspCurSil = next, nextSil
				s.rampMu.Unlock()
			}
			if done {
				break
			}
			time.Sleep(shelbyRampInterval)
		}
	}
}

// writeDSP writes a coefficient block into both adaptive buffers of the
// codec, like APServer: write, flip (SEEK_END), write.
func (s *Shelby) writeDSP(block []byte) bool {
	s.codecMu.Lock()
	defer s.codecMu.Unlock()
	if s.codec == nil {
		f, err := os.OpenFile(shelbyCodecDev, os.O_RDWR, 0)
		if err != nil {
			log.Printf("snd_shelby: open %s: %v", shelbyCodecDev, err)
			return false
		}
		s.codec = f
	}
	for pass := 0; pass < 2; pass++ {
		if _, err := s.codec.Seek(shelbyCoeffOffset, 0); err != nil {
			log.Printf("snd_shelby: codec seek: %v", err)
			return false
		}
		if n, err := s.codec.Write(block); err != nil || n != len(block) {
			log.Printf("snd_shelby: codec write: n=%d err=%v", n, err)
			return false
		}
		if pass == 0 {
			// SEEK_END makes the driver flip the miniDSP's adaptive buffers.
			if _, err := s.codec.Seek(0, 2); err != nil {
				log.Printf("snd_shelby: codec buffer flip: %v", err)
				return false
			}
		}
	}
	return true
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

// dmaRate returns the current estimate of the DMA sample rate in Hz.
func (s *Shelby) dmaRate() float64 {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	return s.rate
}

// readDMACounters reads txDoneCount and txUnderflows from /proc/snd_shelby
// (both count continuously from boot).
func readDMACounters(path string) (dmaCount, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return dmaCount{}, err
	}
	var c dmaCount
	var haveDone, haveUnder bool
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "txDoneCount:"); ok {
			c.done, err = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			haveDone = err == nil
		} else if v, ok := strings.CutPrefix(line, "txUnderflows:"); ok {
			c.under, err = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			haveUnder = err == nil
		}
	}
	if !haveDone || !haveUnder {
		return dmaCount{}, fmt.Errorf("no txDoneCount/txUnderflows in %s", path)
	}
	return c, nil
}

// dmaStats logs once a minute: driver underflows, blocks written, how often
// the writer found the ring empty (producer late), the ring fill range
// (sampled every 250 ms), the longest write and the longest gap between
// Write calls.
func (s *Shelby) dmaStats(stop <-chan struct{}, ring *C.shelby_ring) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	last, err := readDMACounters(s.procPath)
	haveLast := err == nil
	rMin, rMax := shelbyRingBlocks, 0
	next := time.Now().Add(shelbyStatsEvery)
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		s.writeMu.Lock() // ring is freed by Close under writeMu
		if s.ring != ring {
			s.writeMu.Unlock()
			return
		}
		st := C.shelby_ring_stats(ring, 0)
		s.writeMu.Unlock()
		if n := int(st.fill); n < rMin {
			rMin = n
		} else if n > rMax {
			rMax = n
		}
		if time.Now().Before(next) {
			continue
		}
		next = time.Now().Add(shelbyStatsEvery)
		c, err := readDMACounters(s.procPath)
		s.writeMu.Lock()
		if s.ring != ring {
			s.writeMu.Unlock()
			return
		}
		st = C.shelby_ring_stats(ring, 1)
		wrote, starved, wMax := int64(st.wrote), int64(st.starved), time.Duration(st.write_max_ns)
		gMax := s.gapMax
		s.gapMax = 0
		s.writeMu.Unlock()
		if err == nil && haveLast && wrote > 0 {
			log.Printf("snd_shelby: dma stats: %d underflows, %d blocks written, writer starved %d times, ring %d..%d blocks, longest write %v, longest gap between Writes %v",
				c.under-last.under, wrote, starved, rMin, rMax,
				wMax.Round(10*time.Microsecond), gMax.Round(time.Millisecond))
		}
		last, haveLast = c, err == nil
		rMin, rMax = shelbyRingBlocks, 0
	}
}

// rateMeter measures the DMA rate as buffers counted over elapsed time since
// it started, so the estimate keeps getting more precise. The DMA runs
// whether or not anything is written, so playback does not disturb it.
func (s *Shelby) rateMeter(stop <-chan struct{}) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	var t0 time.Time
	var n0 int64
	var lastLog time.Time
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		c, err := readDMACounters(s.procPath)
		n := c.done
		now := time.Now()
		if err != nil {
			if lastLog.IsZero() {
				log.Printf("snd_shelby: rate meter: %v (keeping %.1f Hz)", err, s.dmaRate())
				lastLog = now
			}
			continue
		}
		if t0.IsZero() {
			t0, n0 = now, n
			continue
		}
		dt := now.Sub(t0)
		if dt < shelbyRateMinWindow {
			continue
		}
		r, ok := estimateRate(n-n0, dt)
		if !ok {
			log.Printf("snd_shelby: rate meter: implausible %.1f Hz, restarting the measurement", r)
			t0, n0 = now, n
			continue
		}
		s.rateMu.Lock()
		s.rate = r
		s.rateMu.Unlock()
		if lastLog.IsZero() || now.Sub(lastLog) >= 10*time.Minute {
			log.Printf("snd_shelby: DMA rate %.2f Hz (measured over %.0f s)", r, dt.Seconds())
			lastLog = now
		}
	}
}

// estimateRate turns a buffer count over a time span into Hz.
func estimateRate(buffers int64, dt time.Duration) (float64, bool) {
	r := float64(buffers) * shelbyFrames / dt.Seconds()
	return r, r >= shelbyRateMin && r <= shelbyRateMax
}

// cubicResampler converts interleaved stereo int32 between sample rates with
// 4-point Catmull-Rom interpolation. It carries the last three input frames
// and the fractional read position across calls, so no frames are lost or
// duplicated at chunk boundaries and the output duration is exact (needed
// for sync). The ratio can change between calls (rate meter updates).
type cubicResampler struct {
	step   float64  // input frames per output frame
	pos    float64  // read position in work frames; 1 = first "x0"
	hist   [6]int32 // last 3 input frames, interleaved
	work   []int32
	output []int32
}

func newCubicResampler(inRate int, outRate float64) *cubicResampler {
	r := &cubicResampler{pos: 1}
	r.setRates(inRate, outRate)
	return r
}

func (r *cubicResampler) setRates(inRate int, outRate float64) {
	r.step = float64(inRate) / outRate
}

func clamp24(v float64) int32 {
	if v > 0x7fffff {
		return 0x7fffff
	}
	if v < -0x800000 {
		return -0x800000
	}
	return int32(math.Round(v))
}

func (r *cubicResampler) process(in []int32) []int32 {
	n := len(in) / 2
	w := append(r.work[:0], r.hist[:]...)
	w = append(w, in[:2*n]...)
	last := n + 2 // index of the last work frame
	out := r.output[:0]
	for {
		k := int(r.pos)
		if k+2 > last {
			break
		}
		t := r.pos - float64(k)
		for c := 0; c < 2; c++ {
			xm := float64(w[2*(k-1)+c])
			x0 := float64(w[2*k+c])
			x1 := float64(w[2*(k+1)+c])
			x2 := float64(w[2*(k+2)+c])
			y := x0 + 0.5*t*(x1-xm+t*(2*xm-5*x0+4*x1-x2+t*(3*(x0-x1)+x2-xm)))
			out = append(out, clamp24(y))
		}
		r.pos += r.step
	}
	r.pos -= float64(n)
	copy(r.hist[:], w[2*n:2*n+6])
	r.work, r.output = w, out
	return out
}
