// Package player provides the RAOP audio player implementation.
package player

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kenyonj/airbridge/internal/discovery"
	"github.com/kenyonj/airbridge/pkg/raop"
)

// VolumePassthrough configures external volume control API.
type VolumePassthrough struct {
	URL      string // URL with {{volume}} placeholder
	Method   string // HTTP method (GET, POST, PUT)
	Body     string // Request body with {{volume}} placeholder
	AuthUser string // Basic auth username
	AuthPass string // Basic auth password
}

// RAOPPlayer streams audio to an AirPlay device via RAOP.
type RAOPPlayer struct {
	mu sync.Mutex

	device   *discovery.Device
	streamer *raop.Streamer
	ffmpeg   *exec.Cmd
	cancel   context.CancelFunc

	// Current state
	currentURI string
	volume     int

	// Playback position tracking (drives GetPositionInfo / Seek)
	clock playbackClock

	// Volume passthrough for external API control
	volumePassthrough *VolumePassthrough

	// Volume plugin callback (takes precedence over volumePassthrough)
	volumePluginCallback func(volume int) error
}

// NewRAOPPlayer creates a new RAOP player for the given AirPlay device.
func NewRAOPPlayer(device *discovery.Device) *RAOPPlayer {
	return &RAOPPlayer{
		device: device,
		volume: 80,
	}
}

// SetVolumePassthrough configures external volume control API.
func (p *RAOPPlayer) SetVolumePassthrough(vp *VolumePassthrough) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.volumePassthrough = vp
}

// SetVolumePluginCallback sets a callback for plugin-based volume control.
// This takes precedence over the legacy volumePassthrough configuration.
func (p *RAOPPlayer) SetVolumePluginCallback(cb func(volume int) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.volumePluginCallback = cb
}

// Play starts streaming audio from the given URI to the AirPlay device.
func (p *RAOPPlayer) Play(ctx context.Context, uri string, volume int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.playFrom(ctx, uri, volume, 0)
}

// Seek restarts the stream at position. RAOP has no in-stream seek, so the
// stream is re-opened at the offset instead.
func (p *RAOPPlayer) Seek(ctx context.Context, position time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.currentURI == "" {
		return fmt.Errorf("no media selected")
	}
	if position < 0 {
		position = 0
	}
	return p.playFrom(ctx, p.currentURI, p.volume, position)
}

// Position returns (elapsed, total) for GetPositionInfo.
func (p *RAOPPlayer) Position() (time.Duration, time.Duration) {
	return p.clock.Position()
}

// playFrom starts a stream at the given offset. Caller holds p.mu.
func (p *RAOPPlayer) playFrom(ctx context.Context, uri string, volume int, offset time.Duration) error {
	// Stop any existing stream
	p.stopInternal()

	p.currentURI = uri
	p.volume = volume

	log.Printf("RAOP Play: %s -> %s:%d (volume=%d, offset=%s)", uri, p.device.Host, p.device.Port, volume, offset)

	// Create a new context for this playback
	playCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel

	// Create streamer for the device
	streamer, err := raop.NewStreamer(raop.StreamConfig{
		Host:   p.device.Host,
		Port:   p.device.Port,
		Volume: volume,
	})
	if err != nil {
		cancel()
		return fmt.Errorf("failed to create streamer: %w", err)
	}
	p.streamer = streamer

	ffmpegArgs := buildFFmpegArgs(uri, offset)

	p.ffmpeg = exec.CommandContext(playCtx, "ffmpeg", ffmpegArgs...)
	ffmpegOut, err := p.ffmpeg.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("failed to get ffmpeg stdout: %w", err)
	}

	// Capture stderr for logging - must be set up before Start()
	ffmpegStderr, err := p.ffmpeg.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("failed to get ffmpeg stderr: %w", err)
	}

	if err := p.ffmpeg.Start(); err != nil {
		cancel()
		return fmt.Errorf("failed to start ffmpeg: %w", err)
	}
	log.Printf("Started ffmpeg: ffmpeg %v", ffmpegArgs)

	// Log FFmpeg stderr in background
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := ffmpegStderr.Read(buf)
			if n > 0 {
				log.Printf("[ffmpeg] %s", string(buf[:n]))
			}
			if err != nil {
				break
			}
		}
	}()

	// Exactly one owner of Wait(): stopInternal must never call it, otherwise a
	// seek or a new Play while the old process is still dying deadlocks p.mu.
	go func(c *exec.Cmd) { _ = c.Wait() }(p.ffmpeg)

	// Capture references for the goroutine (these may be replaced by stopInternal)
	streamerRef := p.streamer

	// Start streaming in a goroutine
	go func() {
		// Start the RAOP streamer - this starts cliraop and pipes ffmpegOut to it
		if err := streamerRef.Start(playCtx, ffmpegOut); err != nil {
			if err != context.Canceled && err != io.EOF {
				log.Printf("RAOP streaming error: %v", err)
			}
		}

		// Wait for cliraop to finish playing buffered audio
		_ = streamerRef.WaitForCompletion()
		log.Printf("Playback complete")

		// Now clean up - only nil out if still pointing to our references
		p.mu.Lock()
		if p.streamer == streamerRef {
			p.cancel = nil
			p.ffmpeg = nil
			p.streamer = nil
		}
		p.mu.Unlock()
	}()

	// Position tracking + duration probe (feeds GetPositionInfo / Seek)
	p.clock.Reset()
	p.clock.Start(offset)
	go func(c context.Context, u string) {
		if d := probeDuration(c, u); d > 0 {
			p.clock.SetTotal(d)
			log.Printf("RAOP: media duration %s", formatUPnPTime(d))
		}
	}(playCtx, uri)

	return nil
}

// Pause pauses the current stream.
// Note: RAOP doesn't support true pause, so we just stop.
func (p *RAOPPlayer) Pause(ctx context.Context) error {
	return p.Stop(ctx)
}

// Stop stops the current stream.
func (p *RAOPPlayer) Stop(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	log.Printf("RAOP Stop")
	p.stopInternal()
	return nil
}

// stopInternal stops all resources without locking.
func (p *RAOPPlayer) stopInternal() {
	p.clock.Stop()

	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}

	// Never Wait() here: a dedicated goroutine owns that, and waiting while
	// holding p.mu is what used to wedge every later Seek/Play.
	if p.ffmpeg != nil && p.ffmpeg.Process != nil {
		_ = p.ffmpeg.Process.Kill()
		p.ffmpeg = nil
	}

	if p.streamer != nil {
		_ = p.streamer.Stop()
		p.streamer = nil
	}
}

// SetVolume adjusts the volume.
// Note: RAOP volume is set at connection time. Changes take effect on next Play.
// If volume passthrough is configured, it will also call the external API.
func (p *RAOPPlayer) SetVolume(ctx context.Context, volume int) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	log.Printf("RAOP SetVolume: %d", volume)
	p.volume = volume

	// Plugin callback takes precedence over legacy passthrough
	if p.volumePluginCallback != nil {
		if err := p.volumePluginCallback(volume); err != nil {
			log.Printf("Volume plugin error: %v", err)
			// Don't return error - volume control is best-effort
		}
		return nil
	}

	// Call volume passthrough API if configured (legacy mode)
	if p.volumePassthrough != nil && p.volumePassthrough.URL != "" {
		if err := p.callVolumePassthrough(ctx, volume); err != nil {
			log.Printf("Volume passthrough error: %v", err)
			// Don't return error - passthrough is best-effort
		}
	}

	return nil
}

// callVolumePassthrough makes the HTTP request to the external volume API.
func (p *RAOPPlayer) callVolumePassthrough(ctx context.Context, volume int) error {
	return callVolumePassthrough(ctx, p.volumePassthrough, volume)
}

// callVolumePassthrough is shared by every player implementation.
func callVolumePassthrough(ctx context.Context, vp *VolumePassthrough, volume int) error {
	if vp == nil {
		return nil
	}
	volumeStr := fmt.Sprintf("%d", volume)

	// Replace {{volume}} placeholder in URL and body
	url := strings.ReplaceAll(vp.URL, "{{volume}}", volumeStr)
	body := strings.ReplaceAll(vp.Body, "{{volume}}", volumeStr)

	method := vp.Method
	if method == "" {
		method = "PUT"
	}

	var bodyReader io.Reader
	if body != "" {
		bodyReader = bytes.NewBufferString(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	// Set content type for JSON body
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	// Set basic auth if configured
	if vp.AuthUser != "" {
		req.SetBasicAuth(vp.AuthUser, vp.AuthPass)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API returned %d: %s", resp.StatusCode, string(respBody))
	}

	log.Printf("Volume passthrough: %s %s -> %d", method, url, resp.StatusCode)
	return nil
}

// NullPlayer is a no-op player for testing.
type NullPlayer struct{}

func (NullPlayer) Play(ctx context.Context, uri string, volume int) error {
	log.Printf("[NullPlayer] Play: %s (volume=%d)", uri, volume)
	// Simulate fetching to verify URI works
	resp, err := http.Get(uri)
	if err != nil {
		return fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	n, _ := io.Copy(io.Discard, resp.Body)
	log.Printf("[NullPlayer] Streamed %d bytes", n)
	return nil
}

func (NullPlayer) Pause(ctx context.Context) error {
	log.Printf("[NullPlayer] Pause")
	return nil
}

func (NullPlayer) Stop(ctx context.Context) error {
	log.Printf("[NullPlayer] Stop")
	return nil
}

func (NullPlayer) SetVolume(ctx context.Context, volume int) error {
	log.Printf("[NullPlayer] SetVolume: %d", volume)
	return nil
}

// Seek is a no-op for the NullPlayer.
func (NullPlayer) Seek(ctx context.Context, position time.Duration) error {
	log.Printf("[NullPlayer] Seek: %s", formatUPnPTime(position))
	return nil
}

// Position always reports zero for the NullPlayer.
func (NullPlayer) Position() (time.Duration, time.Duration) {
	return 0, 0
}
