package state

import (
	"context"
	"testing"
)

// A fresh renderer must not push a volume at session start: the receiver
// keeps whatever level it was left at.
func TestStartVolumeIsUnsetByDefault(t *testing.T) {
	s := New(context.Background())
	if got := s.GetStartVolume(); got != 0 {
		t.Fatalf("fresh state must not push a volume, got %d", got)
	}
}

// Once a user (web UI or control point) sets a level, that level is what a
// following Play uses.
func TestStartVolumeAfterExplicitSet(t *testing.T) {
	s := New(context.Background())
	s.SetVolume(35)
	if got := s.GetStartVolume(); got != 35 {
		t.Fatalf("want 35, got %d", got)
	}
}

// Volumes restored from the database or a volume plugin describe the device,
// they are not a user choice, so they must never be pushed at start.
func TestInitialVolumeIsReportedButNotPushed(t *testing.T) {
	s := New(context.Background())
	s.SetVolumeInitial(55)
	if got := s.GetStartVolume(); got != 0 {
		t.Fatalf("restored volume must stay out of the start path, got %d", got)
	}
	if got := s.GetVolume(); got != 55 {
		t.Fatalf("restored volume should still be reported, got %d", got)
	}
}
