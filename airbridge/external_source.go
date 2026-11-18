// SPDX-License-Identifier: LGPL-3.0-or-later
//
// AirBridge extension for doubletake.
// Copyright (C) 2026 LionFari
//
// This file is copied into doubletake/internal/airplay at build time and is
// therefore part of the doubletake library (LGPL-3.0). It lets an embedding
// application feed video and audio that it encoded itself (for example with
// Android's MediaCodec and AudioPlaybackCapture) into doubletake's existing
// AirPlay streaming code instead of launching GStreamer.

package airplay

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// NewExternalScreenCapture returns a ScreenCapture that reads an H.264
// Annex-B byte stream (start-code separated NAL units) from r.
func NewExternalScreenCapture(r io.ReadCloser) *ScreenCapture {
	sc := &ScreenCapture{stdout: r, waitCh: make(chan struct{})}
	var once sync.Once
	sc.cancel = func() { once.Do(func() { close(sc.waitCh) }) }
	return sc
}

// NewExternalAudioCapture returns an AudioCapture that reads interleaved
// 44.1 kHz, stereo, signed 16-bit little-endian PCM from r.
func NewExternalAudioCapture(r io.ReadCloser) *AudioCapture {
	ac := &AudioCapture{pcmPipe: r, waitCh: make(chan struct{})}
	var once sync.Once
	ac.cancel = func() { once.Do(func() { close(ac.waitCh) }) }
	return ac
}

// PCMQueue is a bounded, non-blocking-for-writers byte queue for live audio.
// When the reader falls behind, the oldest audio is discarded so playback
// stays close to real time. It supports read deadlines, which lets
// AudioCapture.DrainStale skip audio that piled up before streaming began.
type PCMQueue struct {
	mu       sync.Mutex
	cond     *sync.Cond
	buf      []byte
	limit    int
	closed   bool
	deadline time.Time
}

// NewPCMQueue creates a queue that keeps at most limit bytes.
func NewPCMQueue(limit int) *PCMQueue {
	if limit < 4096 {
		limit = 4096
	}
	q := &PCMQueue{limit: limit}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Write appends PCM data, dropping the oldest whole sample frames on overflow.
func (q *PCMQueue) Write(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return 0, io.ErrClosedPipe
	}
	q.buf = append(q.buf, p...)
	if over := len(q.buf) - q.limit; over > 0 {
		over += (4 - over%4) % 4 // keep stereo 16-bit frames aligned
		if over > len(q.buf) {
			over = len(q.buf)
		}
		q.buf = append(q.buf[:0:0], q.buf[over:]...)
	}
	q.cond.Broadcast()
	return len(p), nil
}

// Read blocks until data is available, the queue is closed or the read
// deadline passes.
func (q *PCMQueue) Read(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.buf) == 0 {
		if q.closed {
			return 0, io.EOF
		}
		if !q.deadline.IsZero() {
			wait := time.Until(q.deadline)
			if wait <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			t := time.AfterFunc(wait, func() {
				q.mu.Lock()
				q.cond.Broadcast()
				q.mu.Unlock()
			})
			q.cond.Wait()
			t.Stop()
			continue
		}
		q.cond.Wait()
	}
	n := copy(p, q.buf)
	q.buf = q.buf[n:]
	if len(q.buf) == 0 {
		q.buf = q.buf[:0:0]
	}
	return n, nil
}

// SetReadDeadline sets the deadline for future Read calls; zero disables it.
func (q *PCMQueue) SetReadDeadline(t time.Time) error {
	q.mu.Lock()
	q.deadline = t
	q.cond.Broadcast()
	q.mu.Unlock()
	return nil
}

// Close wakes all readers; later writes fail.
func (q *PCMQueue) Close() error {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// Direct per-frame video path
//
// StreamFrames parses a continuous byte stream and can only finish a frame
// once the next one starts, so every frame waits for its successor (on a
// still screen that is the encoder's repeat interval). The app's encoder
// already delivers complete access units, so FrameSender sends each one the
// moment it arrives. It also keeps the delay bounded: if the network falls
// behind, stale frames are discarded together and streaming resumes at the
// next key frame, instead of dropping single P-frames (which leaves the
// picture broken) or queueing ever more delay.

// FrameSenderQueue is how many frames may wait for the network (~100 ms at
// 60 fps) before the waiting ones count as stale.
const FrameSenderQueue = 6

// FrameEvent kinds reported by FrameSender.
const (
	// FrameEventKeyframe: frames were discarded; the encoder should produce a
	// key frame now so the picture continues.
	FrameEventKeyframe = "keyframe"
	// FrameEventCongestion: the network is slower than the video bitrate.
	FrameEventCongestion = "congestion"
	// FrameEventCleared: sending has been smooth again for a while.
	FrameEventCleared = "congestion_cleared"
)

// FrameSender sends complete H.264 access units to a MirrorSession.
type FrameSender struct {
	s      *MirrorSession
	events func(kind, detail string)
	queue  chan []byte
	done   chan struct{}

	mu          sync.Mutex
	closed      bool
	waitKey     bool
	congested   bool
	lastTrouble time.Time
	lastSignal  time.Time

	// statistics since the last Stats call
	sent      int
	sendTotal time.Duration
	sendMax   time.Duration
	dropped   int
}

// NewFrameSender creates a sender for s. events may be nil; it is called from
// the sending goroutine or from Submit and must not block.
func NewFrameSender(s *MirrorSession, events func(kind, detail string)) *FrameSender {
	if events == nil {
		events = func(string, string) {}
	}
	return &FrameSender{
		s:      s,
		events: events,
		queue:  make(chan []byte, FrameSenderQueue),
		done:   make(chan struct{}),
	}
}

// Submit queues one access unit in Annex-B format (SPS/PPS before key
// frames). It never blocks; au is copied.
func (f *FrameSender) Submit(au []byte) error {
	key := annexBIsKeyframe(au)
	var emit [][2]string

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return io.ErrClosedPipe
	}
	if f.waitKey && !key {
		f.dropped++
		f.mu.Unlock()
		return nil
	}
	if key {
		f.waitKey = false
	}
	frame := append([]byte(nil), au...)
	select {
	case f.queue <- frame:
	default:
		// A whole queue is waiting: everything in it is already late.
		stale := 0
	drain:
		for {
			select {
			case <-f.queue:
				stale++
			default:
				break drain
			}
		}
		f.dropped += stale
		if key {
			f.queue <- frame
		} else {
			f.dropped++
			f.waitKey = true
			emit = append(emit, [2]string{FrameEventKeyframe, fmt.Sprintf("%d veraltete Bilder verworfen", stale+1)})
		}
		emit = append(emit, f.troubleLocked(time.Now(), "Warteschlange voll")...)
	}
	f.mu.Unlock()

	for _, e := range emit {
		f.events(e[0], e[1])
	}
	return nil
}

// Run sends queued frames until ctx ends or Close is called.
func (f *FrameSender) Run(ctx context.Context) error {
	s := f.s
	parser := newH264Parser()
	var sps, pps []byte
	primed := false
	count := 0
	// An access unit delimiter after every frame lets the parser hand out the
	// frame's last NAL unit immediately.
	sentinel := []byte{0, 0, 0, 1, 9, 0xf0}

	for {
		var au []byte
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.done:
			return nil
		case au = <-f.queue:
		}

		var vcl []byte
		key := false
		for _, nal := range parser.Push(append(au, sentinel...)) {
			raw := stripStartCode(nal)
			switch nalType(nal) {
			case 7:
				sps = raw
			case 8:
				pps = raw
			case 5:
				key = true
				vcl = append(vcl, avccWrap(raw)...)
			case 1, 2, 3, 4:
				vcl = append(vcl, avccWrap(raw)...)
			}
		}
		if len(vcl) == 0 {
			continue
		}
		if !primed && (!key || sps == nil || pps == nil) {
			continue // the receiver must start with SPS/PPS and a key frame
		}

		start := time.Now()
		ts := s.ntpTimeNow()
		if key && sps != nil && pps != nil {
			if w, h, ok := spsDimensions(sps); ok && (s.videoWidth != w || s.videoHeight != h) {
				dbg("[FRAMES] encoded size %dx%d", w, h)
				s.videoWidth, s.videoHeight = w, h
			}
			if err := s.sendCodecFrame(buildAVCCConfig(sps, pps), ts); err != nil {
				return fmt.Errorf("send codec: %w", err)
			}
			if !primed {
				close(s.firstFrameSent) // starts the data-channel heartbeat
				primed = true
			}
		}
		payload := vcl
		if s.streamCipher != nil {
			payload = s.streamCipher(vcl)
		}
		if err := s.sendFrame(payload, key, ts); err != nil {
			return fmt.Errorf("send frame: %w", err)
		}
		took := time.Since(start)
		if count < 5 {
			dbg("[FRAMES] frame %d key=%v bytes=%d send=%v", count, key, len(vcl), took)
		}
		count++
		f.afterSend(took, len(f.queue))
	}
}

// afterSend tracks timing and reports congestion changes.
func (f *FrameSender) afterSend(took time.Duration, backlog int) {
	now := time.Now()
	var emit [][2]string
	f.mu.Lock()
	f.sent++
	f.sendTotal += took
	if took > f.sendMax {
		f.sendMax = took
	}
	switch {
	case took > 40*time.Millisecond:
		emit = f.troubleLocked(now, fmt.Sprintf("Senden dauerte %d ms", took.Milliseconds()))
	case backlog >= FrameSenderQueue/2:
		emit = f.troubleLocked(now, fmt.Sprintf("%d Bilder warten", backlog))
	case f.congested && now.Sub(f.lastTrouble) > 3*time.Second:
		f.congested = false
		emit = append(emit, [2]string{FrameEventCleared, ""})
	}
	f.mu.Unlock()
	for _, e := range emit {
		f.events(e[0], e[1])
	}
}

// troubleLocked records a sign of congestion; reports it at most every 2 s.
func (f *FrameSender) troubleLocked(now time.Time, detail string) [][2]string {
	f.lastTrouble = now
	if f.congested && now.Sub(f.lastSignal) < 2*time.Second {
		return nil
	}
	f.congested = true
	f.lastSignal = now
	return [][2]string{{FrameEventCongestion, detail}}
}

// Stats returns a summary since the previous call and resets the counters.
func (f *FrameSender) Stats() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	avg := time.Duration(0)
	if f.sent > 0 {
		avg = f.sendTotal / time.Duration(f.sent)
	}
	out := fmt.Sprintf("gesendet %d, Senden Ø %.1f ms, max %d ms, verworfen %d, Warteschlange %d",
		f.sent, float64(avg.Microseconds())/1000, f.sendMax.Milliseconds(), f.dropped, len(f.queue))
	f.sent, f.sendTotal, f.sendMax, f.dropped = 0, 0, 0, 0
	return out
}

// Close stops Run; later Submit calls fail.
func (f *FrameSender) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.done)
	}
}

// annexBIsKeyframe reports whether the first picture slice is an IDR slice.
func annexBIsKeyframe(b []byte) bool {
	for i := 0; i+3 < len(b); i++ {
		if b[i] != 0 || b[i+1] != 0 || b[i+2] != 1 {
			continue
		}
		switch t := b[i+3] & 0x1f; t {
		case 5:
			return true
		case 1, 2, 3, 4:
			return false
		}
		i += 2
	}
	return false
}

// VideoLatency is the playout delay added to video timestamps; the receiver
// shows each frame this long after it was captured.
func (s *MirrorSession) VideoLatency() time.Duration { return s.timestampBias }

// SetVideoLatency changes that delay. Call it before streaming starts. When
// audio is streamed too, video should keep the receiver's audio latency so
// both stay in sync.
func (s *MirrorSession) SetVideoLatency(d time.Duration) {
	if d < 5*time.Millisecond {
		d = 5 * time.Millisecond
	}
	s.timestampBias = d
}
