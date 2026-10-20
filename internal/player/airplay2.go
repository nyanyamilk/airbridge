package player

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kenyonj/airbridge/internal/discovery"
)

// AirPlay2Player streams to a device using AirPlay 2 (native HAP flow with
// transient pairing) through the cliairplay binary.
//
// Why this exists: HomePod OS 27 silently drops third-party AirPlay 1 (RAOP)
// audio, while its AirPlay 2 path works. RAOP remains the default; this is
// selected with AIRBRIDGE_SINK=ap2 (or =auto for AudioAccessory models).
type AirPlay2Player struct {
	mu sync.Mutex

	device  *discovery.Device
	binPath string

	ffmpeg  *exec.Cmd
	cliproc *exec.Cmd
	fifo    string
	cancel  context.CancelFunc

	clock playbackClock

	currentURI string
	volume     int

	volumePassthrough    *VolumePassthrough
	volumePluginCallback func(volume int) error
}

// AirPlay2Binary returns the cliairplay path and whether it is usable.
func AirPlay2Binary() (string, bool) {
	p, err := findCLIAirplay()
	if err != nil {
		return "", false
	}
	return p, true
}

func findCLIAirplay() (string, error) {
	if p := os.Getenv("CLIAIRPLAY_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	candidates := []string{
		"./bin/cliairplay",
		"/opt/cli/cliairplay",
		"/usr/local/bin/cliairplay",
		"/usr/bin/cliairplay",
	}
	for _, c := range candidates {
		if abs, err := filepath.Abs(c); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs, nil
			}
		}
	}
	if p, err := exec.LookPath("cliairplay"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("cliairplay binary not found (set CLIAIRPLAY_PATH)")
}

// NewAirPlay2Player creates an AirPlay 2 player for the given device.
func NewAirPlay2Player(device *discovery.Device) (*AirPlay2Player, error) {
	bin, err := findCLIAirplay()
	if err != nil {
		return nil, err
	}
	return &AirPlay2Player{device: device, binPath: bin, volume: 100}, nil
}

// SetVolumePassthrough configures external volume control API.
func (p *AirPlay2Player) SetVolumePassthrough(vp *VolumePassthrough) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.volumePassthrough = vp
}

// SetVolumePluginCallback sets a callback for plugin-based volume control.
func (p *AirPlay2Player) SetVolumePluginCallback(cb func(volume int) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.volumePluginCallback = cb
}

// Play starts streaming from uri.
func (p *AirPlay2Player) Play(ctx context.Context, uri string, volume int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.playFrom(ctx, uri, volume, 0)
}

// Seek restarts the stream at the given position. AirPlay 2 has no
// in-stream seek, so this is a fresh session anchored at the new offset.
func (p *AirPlay2Player) Seek(ctx context.Context, position time.Duration) error {
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

// Position returns (elapsed, total).
func (p *AirPlay2Player) Position() (time.Duration, time.Duration) {
	return p.clock.Position()
}

func (p *AirPlay2Player) playFrom(ctx context.Context, uri string, volume int, offset time.Duration) error {
	p.stopInternal()

	playCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.currentURI = uri
	p.volume = volume

	args := buildFFmpegArgs(uri, offset)
	log.Printf("AP2 Play: %s -> %s:%d (volume=%d, offset=%s)", uri, p.device.Host, p.device.Port, volume, offset)
	log.Printf("AP2 ffmpeg: ffmpeg %v", args)

	ff := exec.CommandContext(playCtx, "ffmpeg", args...)
	pcm, err := ff.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("ffmpeg stdout: %w", err)
	}
	ferr, err := ff.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("ffmpeg stderr: %w", err)
	}
	if err := ff.Start(); err != nil {
		cancel()
		return fmt.Errorf("failed to start ffmpeg: %w", err)
	}
	p.ffmpeg = ff
	go logLines(ferr, "[ffmpeg]")
	// Single owner of Wait() so a later Stop cannot double-wait (deadlock).
	go func(c *exec.Cmd) { _ = c.Wait() }(ff)

	fifo := filepath.Join(os.TempDir(),
		fmt.Sprintf("airbridge-ap2-%d-%d.fifo", os.Getpid(), time.Now().UnixNano()))
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		cancel()
		return fmt.Errorf("mkfifo: %w", err)
	}
	p.fifo = fifo

	cli := exec.CommandContext(playCtx, p.binPath,
		"--protocol", "airplay2",
		"--ap2-native",
		"--port", strconv.Itoa(p.device.Port),
		"--volume", strconv.Itoa(volume),
		"--name", "Airbridge",
		"--cmdpipe", fifo,
		p.device.Host,
	)
	cli.Stdin = pcm
	cout, err := cli.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("cliairplay stdout: %w", err)
	}
	cerr, err := cli.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("cliairplay stderr: %w", err)
	}
	if err := cli.Start(); err != nil {
		cancel()
		return fmt.Errorf("failed to start cliairplay: %w", err)
	}
	p.cliproc = cli

	// cliairplay only plays after an explicit START command, which requires a
	// live session, so watch its status lines first.
	connected := make(chan struct{})
	var once sync.Once
	scan := func(r io.Reader) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 8192), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			log.Printf("[cliairplay] %s", line)
			if strings.Contains(line, "[STATUS] connected") {
				once.Do(func() { close(connected) })
			}
		}
	}
	go scan(cout)
	go scan(cerr)

	go func() {
		select {
		case <-connected:
			f, err := os.OpenFile(fifo, os.O_RDWR, 0)
			if err != nil {
				log.Printf("AP2: cannot open cmdpipe: %v", err)
				return
			}
			defer f.Close()
			ms := time.Now().Add(800 * time.Millisecond).UnixMilli()
			if _, err := fmt.Fprintf(f, "START_UNIX_MS=%d\nACTION=START\n", ms); err != nil {
				log.Printf("AP2: START failed: %v", err)
				return
			}
			log.Printf("AP2: START scheduled at unix_ms=%d", ms)
		case <-playCtx.Done():
		}
	}()

	p.clock.Reset()
	p.clock.Start(offset)
	go func(c context.Context, u string) {
		if d := probeDuration(c, u); d > 0 {
			p.clock.SetTotal(d)
			log.Printf("AP2: media duration %s", formatUPnPTime(d))
		}
	}(playCtx, uri)

	cliRef := cli
	go func() {
		err := cliRef.Wait()
		log.Printf("AP2 sink exited: %v", err)
		p.mu.Lock()
		if p.cliproc == cliRef {
			p.cliproc = nil
		}
		p.mu.Unlock()
	}()

	return nil
}

// Pause stops the stream (AirPlay 2 pause is not supported by the CLI).
func (p *AirPlay2Player) Pause(ctx context.Context) error {
	return p.Stop(ctx)
}

// Stop tears the session down.
func (p *AirPlay2Player) Stop(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	log.Printf("AP2 Stop")
	p.stopInternal()
	return nil
}

// stopInternal releases resources. Caller holds p.mu.
func (p *AirPlay2Player) stopInternal() {
	p.clock.Stop()

	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	for _, c := range []*exec.Cmd{p.cliproc, p.ffmpeg} {
		if c != nil && c.Process != nil {
			_ = c.Process.Kill()
		}
	}
	p.cliproc = nil
	p.ffmpeg = nil

	if p.fifo != "" {
		_ = os.Remove(p.fifo)
		p.fifo = ""
	}
}

// SetVolume stores the level (applied on the next Play) and forwards it to a
// plugin or passthrough endpoint when configured.
func (p *AirPlay2Player) SetVolume(ctx context.Context, volume int) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	log.Printf("AP2 SetVolume: %d", volume)
	p.volume = volume

	if p.volumePluginCallback != nil {
		if err := p.volumePluginCallback(volume); err != nil {
			log.Printf("Volume plugin error: %v", err)
		}
		return nil
	}
	if p.volumePassthrough != nil && p.volumePassthrough.URL != "" {
		if err := callVolumePassthrough(ctx, p.volumePassthrough, volume); err != nil {
			log.Printf("Volume passthrough error: %v", err)
		}
	}
	return nil
}

func logLines(r io.Reader, prefix string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 1<<20)
	for sc.Scan() {
		log.Printf("%s %s", prefix, sc.Text())
	}
}
