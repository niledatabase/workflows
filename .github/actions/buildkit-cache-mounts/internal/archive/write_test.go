package archive

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteDirectoryReportsUnreadableChild(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read files without permission bits")
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "unreadable"), []byte("value"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := WriteDirectory(t.Context(), io.Discard, source); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("WriteDirectory returned %v, want permission error", err)
	}
}

func TestWriteDirectoryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := WriteDirectory(ctx, io.Discard, t.TempDir())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteDirectory returned %v", err)
	}
}
