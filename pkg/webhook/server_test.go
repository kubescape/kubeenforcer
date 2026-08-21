package webhook

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestNotifyChangesDetectsModification(t *testing.T) {
	path := t.TempDir() + "/watched"
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	changes := notifyChanges(ctx, path)

	// let the first snapshot settle before we change anything
	time.Sleep(100 * time.Millisecond)

	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	select {
	case <-changes:
	case <-time.After(4 * time.Second):
		t.Fatal("notifyChanges never reported the mtime change")
	}
}
