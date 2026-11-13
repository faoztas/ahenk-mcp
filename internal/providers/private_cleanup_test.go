// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/faoztas/ahenk-mcp/internal/execution"
)

func TestOpenCodeCleanupMarkerAfterSessionRemoval(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "attempt")
	if err := execution.NewAttemptDir(dir); err != nil {
		t.Fatal(err)
	}
	isolated, err := os.MkdirTemp(dir, "oc-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(isolated, "synthetic-session"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	lease, err := execution.OpenWorkerLease(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := cleanupOpenCodeSession(dir, string([]byte{0}), lease); !errors.Is(err, execution.ErrCleanup) {
		t.Fatalf("removal failure was not quarantined: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".worker.cleaned")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("premature cleanup marker: %v", err)
	}
	if err := cleanupOpenCodeSession(dir, isolated, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(isolated); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolated session remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".worker.cleaned")); err != nil {
		t.Fatalf("missing cleanup marker after removal: %v", err)
	}
}
