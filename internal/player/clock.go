package player

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// leadSilence is the silence prepended to every stream so the AirPlay
// receiver's buffer fills before real audio starts. Reported positions
// subtract it, otherwise the UI drifts ahead by that amount.
const leadSilence = 1500 * time.Millisecond

// playbackClock tracks the playback position of a stream.
//
// The position is derived from wall clock rather than from the sink: sinks
// only know how many bytes they were handed, and that number keeps advancing
// after the receiver has actually stopped.
type playbackClock struct {
	mu      sync.Mutex
	base    time.Duration // position the stream was started from (seek target)
	total   time.Duration // media duration, 0 = unknown
	started time.Time
	running bool
}

// Start begins tracking from the given offset.
func (c *playbackClock) Start(offset time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if offset < 0 {
		offset = 0
	}
	c.base = offset
	c.started = time.Now()
	c.running = true
}

// Stop freezes the position where playback currently is.
func (c *playbackClock) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.base = c.elapsedLocked()
	c.running = false
}

// Reset forgets everything (new media).
func (c *playbackClock) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.base, c.total = 0, 0
	c.running = false
}

// SetTotal records the media duration once a probe completes.
func (c *playbackClock) SetTotal(d time.Duration) {
	if d <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total = d
}

// Base returns the offset the current stream started from.
func (c *playbackClock) Base() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.base
}

// Position returns (elapsed, total). total is 0 when unknown.
func (c *playbackClock) Position() (time.Duration, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.elapsedLocked(), c.total
}

func (c *playbackClock) elapsedLocked() time.Duration {
	if !c.running {
		return c.base
	}
	played := time.Since(c.started) - leadSilence
	if played < 0 {
		played = 0
	}
	d := c.base + played
	if c.total > 0 && d > c.total {
		d = c.total
	}
	return d
}

// probeDuration asks ffprobe for the media duration. Best effort: local files
// and HTTP URLs both work, but a stream without a known length returns 0.
func probeDuration(ctx context.Context, uri string) time.Duration {
	if uri == "" {
		return 0
	}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	out, err := exec.CommandContext(cctx, "ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		uri).Output()
	if err != nil {
		return 0
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || f <= 0 {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}

// formatUPnPTime renders a duration the way UPnP expects: HH:MM:SS.
func formatUPnPTime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int64(d / time.Second)
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, (s%3600)/60, s%60)
}

// parseUPnPTime parses HH:MM:SS, MM:SS, SS or a raw second count. The last
// component may carry a fraction, which some control points send.
func parseUPnPTime(v string) (time.Duration, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return 0, fmt.Errorf("empty time")
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, fmt.Errorf("invalid time %q", v)
	}

	nums := make([]float64, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid time %q", v)
		}
		if f < 0 {
			return 0, fmt.Errorf("negative time %q", v)
		}
		nums[i] = f
	}

	var d time.Duration
	switch len(nums) {
	case 1:
		d = time.Duration(nums[0] * float64(time.Second))
	case 2:
		d = time.Duration(nums[0])*time.Minute + time.Duration(nums[1]*float64(time.Second))
	case 3:
		d = time.Duration(nums[0])*time.Hour +
			time.Duration(nums[1])*time.Minute +
			time.Duration(nums[2]*float64(time.Second))
	}
	return d, nil
}

// buildFFmpegArgs decodes any input to s16le/44.1k/stereo, prepending
// leadSilence so the receiver has something to buffer while it connects.
// offset > 0 seeks in the input before decoding.
func buildFFmpegArgs(uri string, offset time.Duration) []string {
	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-f", "lavfi", "-i", fmt.Sprintf("anullsrc=r=44100:cl=stereo:d=%g",
			leadSilence.Seconds()),
		"-reconnect", "1",
		"-reconnect_streamed", "1",
		"-reconnect_delay_max", "5",
	}
	if offset > 0 {
		args = append(args, "-ss", strconv.FormatFloat(offset.Seconds(), 'f', 3, 64))
	}
	args = append(args,
		"-i", uri,
		"-filter_complex", "[0][1]concat=n=2:v=0:a=1",
		"-f", "s16le",
		"-ar", "44100",
		"-ac", "2",
		"-acodec", "pcm_s16le",
		"-",
	)
	return args
}
