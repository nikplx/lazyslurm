package history

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAppendAndList(t *testing.T) {
	dir := t.TempDir()
	s := New(filepath.Join(dir, "history.json"))
	if subs, err := s.List(); err != nil || len(subs) != 0 {
		t.Fatalf("fresh list: %v %v", subs, err)
	}
	if _, err := s.Append(Submission{Command: "echo hi", JobName: "a", JobIDs: []string{"1"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := s.Append(Submission{Command: "echo bye", JobName: "b", JobIDs: []string{"2"}}); err != nil {
		t.Fatal(err)
	}
	subs, err := s.List()
	if err != nil || len(subs) != 2 {
		t.Fatalf("got %v %v", subs, err)
	}
	if subs[0].Command != "echo bye" {
		t.Fatalf("newest first, got %q", subs[0].Command)
	}
	// Reload from disk via a new store.
	s2 := New(filepath.Join(dir, "history.json"))
	subs2, err := s2.List()
	if err != nil || len(subs2) != 2 || subs2[1].JobIDs[0] != "1" {
		t.Fatalf("reload: %v %v", subs2, err)
	}
}
