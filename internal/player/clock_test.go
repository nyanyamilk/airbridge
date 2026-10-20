package player

import (
	"testing"
	"time"
)

func TestFormatUPnPTime(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "00:00:00"},
		{5 * time.Second, "00:00:05"},
		{90 * time.Second, "00:01:30"},
		{time.Hour + 2*time.Minute + 3*time.Second, "01:02:03"},
		{-5 * time.Second, "00:00:00"},
	}
	for _, c := range cases {
		if got := formatUPnPTime(c.in); got != c.want {
			t.Errorf("formatUPnPTime(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseUPnPTime(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"00:00:10", 10 * time.Second},
		{"01:02:03", time.Hour + 2*time.Minute + 3*time.Second},
		{"02:30", 2*time.Minute + 30*time.Second},
		{"45", 45 * time.Second},
		{"12.5", 12500 * time.Millisecond},
		{"00:00:12.5", 12500 * time.Millisecond},
	}
	for _, c := range cases {
		got, err := parseUPnPTime(c.in)
		if err != nil {
			t.Fatalf("parseUPnPTime(%q) error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("parseUPnPTime(%q) = %v, want %v", c.in, got, c.want)
		}
	}

	for _, bad := range []string{"", "abc", "-1", "1:2:3:4"} {
		if _, err := parseUPnPTime(bad); err == nil {
			t.Errorf("parseUPnPTime(%q) expected error", bad)
		}
	}
}

func TestPlaybackClock(t *testing.T) {
	var c playbackClock
	c.Start(30 * time.Second)
	pos, total := c.Position()
	if total != 0 {
		t.Errorf("total = %v, want 0 (unknown)", total)
	}
	// Just started: the prepended silence has not elapsed yet, so the
	// content position is still the seek base.
	if pos != 30*time.Second {
		t.Errorf("pos = %v, want 30s", pos)
	}

	c.SetTotal(2 * time.Minute)
	if _, total := c.Position(); total != 2*time.Minute {
		t.Errorf("total = %v, want 2m", total)
	}

	c.Stop()
	frozen := c.Base()
	if p, _ := c.Position(); p != frozen {
		t.Errorf("stop did not freeze position: %v vs %v", p, frozen)
	}
	time.Sleep(10 * time.Millisecond)
	if p, _ := c.Position(); p != frozen {
		t.Errorf("position moved after stop: %v -> %v", frozen, p)
	}

	c.Reset()
	if p, tot := c.Position(); p != 0 || tot != 0 {
		t.Errorf("reset left %v/%v", p, tot)
	}
}

func TestBuildFFmpegArgsSeek(t *testing.T) {
	plain := buildFFmpegArgs("http://x/y.mp3", 0)
	for _, a := range plain {
		if a == "-ss" {
			t.Fatalf("offset 0 must not add -ss: %v", plain)
		}
	}

	seeked := buildFFmpegArgs("http://x/y.mp3", 90*time.Second)
	foundSS, foundURI := false, false
	for i, a := range seeked {
		if a == "-ss" && i+1 < len(seeked) {
			foundSS = true
			if seeked[i+1] != "90.000" {
				t.Errorf("-ss value = %q, want 90.000", seeked[i+1])
			}
			if i+2 >= len(seeked) || seeked[i+2] != "-i" {
				t.Errorf("-ss must precede -i, got %v", seeked[i:])
			}
		}
		if a == "http://x/y.mp3" {
			foundURI = true
		}
	}
	if !foundSS || !foundURI {
		t.Errorf("seek args malformed: ss=%v uri=%v (%v)", foundSS, foundURI, seeked)
	}
}
