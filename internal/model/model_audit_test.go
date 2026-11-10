// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLoadModelInventoryRequiresPrivateRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inventory.json")
	data := []byte(`{"fixture":"go-integer-v1","observed_at":"2026-01-01T00:00:00Z","models":[]}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadModelInventory(path); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0644, 0620} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadModelInventory(path); err == nil {
			t.Fatalf("accepted shared inventory mode %o", mode)
		}
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	symlink, hardlink, fifo := filepath.Join(dir, "symlink"), filepath.Join(dir, "hardlink"), filepath.Join(dir, "fifo")
	for _, err := range []error{os.Symlink(path, symlink), os.Link(path, hardlink), syscall.Mkfifo(fifo, 0600)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, unsafe := range []string{symlink, hardlink, path, fifo, dir} {
		if _, err := LoadModelInventory(unsafe); err == nil {
			t.Fatalf("accepted unsafe inventory %s", filepath.Base(unsafe))
		}
	}
}
