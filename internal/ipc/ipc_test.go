// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package ipc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDaemonSocketBindAndPermissions(t *testing.T) {
	dir := t.TempDir()
	srv := mcp.NewServer(&mcp.Implementation{Name: "ahenk", Version: "test"}, nil)
	d, err := StartDaemon(context.Background(), dir, srv)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	info, err := os.Stat(SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("perm: %v", info.Mode().Perm())
	}
}

func TestStaleSocketRecovery(t *testing.T) {
	dir := t.TempDir()
	sock := SocketPath(dir)
	if err := os.WriteFile(sock, []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "ahenk", Version: "test"}, nil)
	d, err := StartDaemon(context.Background(), dir, srv)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
}

func TestProxyBridgeWithMockServer(t *testing.T) {
	dir := t.TempDir()
	// mock server socket
	srv := mcp.NewServer(&mcp.Implementation{Name: "ahenk", Version: "test"}, nil)
	d, err := StartDaemon(context.Background(), dir, srv)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Use a pipe to simulate stdio
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	r2, w2, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	defer w2.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = RunProxy(ctx, dir, r, w2)
	}()

	// Just ensure connection succeeds by dialing
	conn, err := dialUnix(dir, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	_ = filepath.Base
	_ = net.Listener(nil)
}
