// SPDX-License-Identifier: LGPL-3.0-or-later
//
// AirBridge mobile binding for doubletake.
// Copyright (C) 2026 LionFari
//
// This package is copied into the doubletake module at build time and bound
// with gomobile into airplaylib.aar. It is part of the doubletake-based
// library and is licensed under the LGPL-3.0 like doubletake itself. The
// application talks to it only through this small API, so the library can
// be rebuilt and replaced independently of the app.
//
// Typical use from the app:
//
//	s := abcore.NewSession(host, port, credentialFile, listener)
//	s.Connect()                  // may call listener.OnStatus("pin", …)
//	s.StartMirror(30, 6000, true)
//	s.WriteVideo(annexB) …       // from the hardware encoder
//	s.WriteAudio(pcm) …          // 44.1 kHz stereo S16LE
//	s.Close()
package abcore

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"doubletake/internal/airplay"
)

// Listener receives status changes and diagnostic log lines.
// Calls may come from any goroutine.
type Listener interface {
	// OnStatus reports a state: "connecting", "pin", "paired", "streaming",
	// "ended" or "error". While streaming it also reports "congestion" (the
	// network is slower than the bitrate), "congestion_cleared" and
	// "keyframe" (late frames were discarded; encode a key frame now).
	// detail carries a human-readable message.
	OnStatus(state string, detail string)
	// OnLog forwards doubletake's diagnostic output.
	OnLog(line string)
}

// SetVerbose enables doubletake's detailed debug logging.
func SetVerbose(on bool) { airplay.DebugMode = on }

// SetTargetLatencyMs adjusts the receiver-side buffering target.
func SetTargetLatencyMs(ms int) {
	if ms > 0 {
		airplay.SetTargetLatency(time.Duration(ms) * time.Millisecond)
	}
}

// lowVideoLatency is the picture delay used when no audio is streamed: just
// enough margin for clock differences between phone and receiver.
const lowVideoLatency = 40 * time.Millisecond

type logBridge struct{ l Listener }

func (b logBridge) Write(p []byte) (int, error) {
	if b.l == nil {
		return len(p), nil
	}
	line := strings.TrimRight(string(p), "\n")
	b.l.OnLog(line)
	return len(p), nil
}

// Session is one connection to an AirPlay receiver.
type Session struct {
	host     string
	port     int
	credFile string
	listener Listener

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	client   *airplay.AirPlayClient
	info     *airplay.ReceiverInfo
	mirror   *airplay.MirrorSession
	frames   *airplay.FrameSender
	pcm      *airplay.PCMQueue
	audioCap *airplay.AudioCapture
	pinCh    chan string
	closed   bool
}

// NewSession prepares a session. credentialFile is a writable path where
// long-term pairing keys are kept (one file for all receivers).
func NewSession(host string, port int, credentialFile string, l Listener) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	if l != nil {
		log.SetFlags(0)
		log.SetOutput(logBridge{l})
	}
	return &Session{
		host: host, port: port, credFile: credentialFile, listener: l,
		ctx: ctx, cancel: cancel,
	}
}

func (s *Session) status(state, detail string) {
	if s.listener != nil {
		s.listener.OnStatus(state, detail)
	}
}

func (s *Session) dial() (*airplay.AirPlayClient, *airplay.ReceiverInfo, error) {
	c := airplay.NewAirPlayClient(s.host, s.port)
	if err := c.Connect(s.ctx); err != nil {
		return nil, nil, fmt.Errorf("verbinden: %w", err)
	}
	info, err := c.GetInfo()
	if err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("geräteinfo: %w", err)
	}
	return c, info, nil
}

// Connect opens the connection and authenticates. Order of attempts:
// stored keys (pair-verify), transient pairing, then PIN pairing. For PIN
// pairing it reports status "pin" and waits for SubmitPin or CancelPin.
func (s *Session) Connect() error {
	s.status("connecting", s.host)
	store, err := airplay.NewCredentialStore(s.credFile)
	if err != nil {
		return fmt.Errorf("schlüsselspeicher: %w", err)
	}

	c, info, err := s.dial()
	if err != nil {
		return err
	}

	authenticated := false
	if saved := store.Lookup(info.DeviceID); saved != nil && saved.HasPairingCredentials() {
		pub, priv := saved.Ed25519Keys()
		c.PairingID = saved.PairingID
		c.PairKeys = &airplay.PairKeys{Ed25519Public: pub, Ed25519Private: priv}
		if err := c.PairVerify(s.ctx); err == nil {
			authenticated = true
		} else {
			log.Printf("[abcore] stored keys rejected: %v", err)
			c.Close()
			if c, info, err = s.dial(); err != nil {
				return err
			}
		}
	}

	if !authenticated {
		if err := c.Pair(s.ctx, ""); err == nil {
			authenticated = true
		} else {
			log.Printf("[abcore] transient pairing failed: %v", err)
		}
	}

	if !authenticated {
		if err := c.StartPINDisplay(); err != nil {
			// Without a code on screen the user could never finish pairing.
			log.Printf("[abcore] pin display: %v", err)
			c.Close()
			return errors.New("der Empfänger lässt keine Kopplung zu – prüfe auf dem Apple TV unter " +
				"Einstellungen › AirPlay und HomeKit, wer AirPlay verwenden darf")
		}
		pin, err := s.waitForPin()
		if err != nil {
			c.Close()
			return err
		}
		c.Close()
		if c, info, err = s.dial(); err != nil {
			return err
		}
		if err := c.Pair(s.ctx, pin); err != nil {
			c.Close()
			return fmt.Errorf("PIN abgelehnt: %w", err)
		}
		if err := store.Save(info.DeviceID, c.PairingID, c.PairKeys.Ed25519Public, c.PairKeys.Ed25519Private); err != nil {
			log.Printf("[abcore] could not save keys: %v", err)
		}
	}

	s.mu.Lock()
	s.client, s.info = c, info
	s.mu.Unlock()
	s.status("paired", info.Name)
	return nil
}

func (s *Session) waitForPin() (string, error) {
	ch := make(chan string, 1)
	s.mu.Lock()
	s.pinCh = ch
	s.mu.Unlock()
	s.status("pin", "")
	select {
	case pin, ok := <-ch:
		if !ok || pin == "" {
			return "", errors.New("kopplung abgebrochen")
		}
		return pin, nil
	case <-s.ctx.Done():
		return "", errors.New("kopplung abgebrochen")
	case <-time.After(3 * time.Minute):
		return "", errors.New("keine PIN eingegeben")
	}
}

// SubmitPin answers a "pin" status with the code shown on the TV.
// Safe to call from any thread; ignored if no PIN is pending.
func (s *Session) SubmitPin(pin string) {
	s.mu.Lock()
	ch := s.pinCh
	s.pinCh = nil
	s.mu.Unlock()
	if ch != nil {
		ch <- strings.TrimSpace(pin)
	}
}

// CancelPin aborts a pending PIN request.
func (s *Session) CancelPin() {
	s.mu.Lock()
	ch := s.pinCh
	s.pinCh = nil
	s.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

// ReceiverName returns the receiver's display name after Connect.
func (s *Session) ReceiverName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.info == nil {
		return ""
	}
	return s.info.Name
}

// ReceiverDeviceID returns the receiver's AirPlay device id after Connect.
func (s *Session) ReceiverDeviceID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.info == nil {
		return ""
	}
	return s.info.DeviceID
}

// ReceiverWidth is the width of the receiver's display in pixels (0 if unknown).
func (s *Session) ReceiverWidth() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, _ := s.info.DisplaySize()
	return w
}

// ReceiverHeight is the height of the receiver's display in pixels (0 if unknown).
func (s *Session) ReceiverHeight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, h := s.info.DisplaySize()
	return h
}

// StartMirror sets up the screen-mirroring stream. After it returns, the
// app feeds encoded video with WriteVideo and, if HasAudio, PCM with
// WriteAudio. Streaming problems are reported as status "error"/"ended".
func (s *Session) StartMirror(fps int, bitrateKbps int, audio bool) error {
	s.mu.Lock()
	c := s.client
	s.mu.Unlock()
	if c == nil {
		return errors.New("nicht verbunden")
	}

	if c.FpEkey == nil {
		if err := c.FairPlaySetup(s.ctx); err != nil && !errors.Is(err, airplay.ErrFairPlayUnsupported) {
			return fmt.Errorf("fairplay: %w", err)
		}
	}

	mirror, err := c.SetupMirror(s.ctx, airplay.StreamConfig{
		FPS:     fps,
		Bitrate: bitrateKbps,
		NoAudio: !audio,
	})
	if err != nil {
		return fmt.Errorf("spiegelung einrichten: %w", err)
	}

	// Without sound there is nothing to stay in sync with, so the picture is
	// shown right away instead of after the receiver's audio buffer delay.
	if lat := mirror.VideoLatency(); !(audio && mirror.HasAudio()) && lat > lowVideoLatency {
		mirror.SetVideoLatency(lowVideoLatency)
	}
	log.Printf("[abcore] Bildverzögerung am Empfänger: %d ms", mirror.VideoLatency().Milliseconds())

	frames := airplay.NewFrameSender(mirror, func(kind, detail string) { s.status(kind, detail) })

	s.mu.Lock()
	s.mirror, s.frames = mirror, frames
	s.mu.Unlock()

	if audio && mirror.HasAudio() {
		q := airplay.NewPCMQueue(44100 * 4 / 4) // ~250 ms
		ac := airplay.NewExternalAudioCapture(q)
		s.mu.Lock()
		s.pcm, s.audioCap = q, ac
		s.mu.Unlock()
		go func() {
			if err := mirror.StreamAudio(s.ctx, ac, mirror.AudioStream()); err != nil && s.ctx.Err() == nil {
				log.Printf("[abcore] audio stream: %v", err)
			}
		}()
	}

	go func() {
		err := frames.Run(s.ctx)
		if s.ctx.Err() != nil {
			return
		}
		if err != nil {
			s.status("error", err.Error())
		} else {
			s.status("ended", "")
		}
	}()

	s.status("streaming", s.info.Name)
	return nil
}

// HasAudio reports whether an audio stream is active.
func (s *Session) HasAudio() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pcm != nil
}

// WriteVideo passes one complete H.264 access unit in Annex-B format
// (SPS/PPS before every key frame). It never blocks: if the network falls
// behind, late frames are discarded and a "keyframe" status asks for a new
// key frame.
func (s *Session) WriteVideo(data []byte) error {
	s.mu.Lock()
	f := s.frames
	s.mu.Unlock()
	if f == nil {
		return errors.New("stream nicht aktiv")
	}
	return f.Submit(data)
}

// VideoStats summarizes sending since the previous call (for the app log).
func (s *Session) VideoStats() string {
	s.mu.Lock()
	f := s.frames
	s.mu.Unlock()
	if f == nil {
		return ""
	}
	return f.Stats()
}

// WriteAudio queues PCM audio (44.1 kHz, stereo, S16LE). Never blocks;
// old audio is dropped if the stream falls behind.
func (s *Session) WriteAudio(pcm []byte) error {
	s.mu.Lock()
	q := s.pcm
	s.mu.Unlock()
	if q == nil {
		return nil
	}
	_, err := q.Write(pcm)
	return err
}

// SetAudioMuted mutes or unmutes the receiver's audio stream.
func (s *Session) SetAudioMuted(muted bool) error {
	s.mu.Lock()
	m := s.mirror
	s.mu.Unlock()
	if m == nil {
		return nil
	}
	return m.SetAudioMuted(muted)
}

// Close ends streaming and the connection. Safe to call more than once.
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	ch := s.pinCh
	s.pinCh = nil
	frames, audioCap, mirror, client := s.frames, s.audioCap, s.mirror, s.client
	s.mu.Unlock()

	if ch != nil {
		close(ch)
	}
	s.cancel()
	if frames != nil {
		frames.Close()
	}
	if audioCap != nil {
		audioCap.Stop()
	}
	if mirror != nil {
		mirror.Close()
	}
	if client != nil {
		client.Close()
	}
}
