// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package ipc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDaemonCloseRemovesAbsoluteSocketPath(t *testing.T) {
	stateDir := t.TempDir()
	otherDir := t.TempDir()

	srv := mcp.NewServer(&mcp.Implementation{Name: "ahenk", Version: "test"}, nil)
	d, err := StartDaemon(context.Background(), stateDir, srv)
	if err != nil {
		t.Fatalf("StartDaemon: %v", err)
	}

	conn, err := dialUnix(stateDir, 2*time.Second)
	if err != nil {
		_ = d.Close()
		t.Fatalf("dialUnix: %v", err)
	}
	defer conn.Close()

	// Wait briefly until the daemon registers the active client.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if d.ActiveClients() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := d.ActiveClients(); got != 1 {
		t.Fatalf("expected ActiveClients() == 1, got %d", got)
	}

	// Switch working directory before calling Close().
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(otherDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(wd)
	})

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- d.Close()
	}()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Daemon.Close returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Daemon.Close hung with active idle client")
	}

	// Socket file in stateDir must be removed even when Close() ran from otherDir.
	if _, err := os.Stat(SocketPath(stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected socket file %q to be removed, got stat err: %v", SocketPath(stateDir), err)
	}

	// Client connection must be closed promptly.
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected client connection to be closed after Daemon.Close")
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatalf("client connection remained open after Daemon.Close: %v", err)
	}
}

func TestRunProxy_ContextCancelDoesNotHang(t *testing.T) {
	stateDir := t.TempDir()
	ln, err := listenUnix(stateDir)
	if err != nil {
		t.Fatalf("listenUnix: %v", err)
	}
	defer ln.Close()
	defer os.Remove(SocketPath(stateDir))

	// Accept one silent connection and hold it open until test cleanup.
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	t.Cleanup(func() {
		select {
		case c := <-accepted:
			_ = c.Close()
		default:
		}
	})

	pr, pw := io.Pipe()
	t.Cleanup(func() {
		_ = pw.Close()
		_ = pr.Close()
	})
	var out bytes.Buffer

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunProxy(ctx, stateDir, pr, &out)
	}()

	// Wait until connection is accepted by the silent server.
	select {
	case c := <-accepted:
		t.Cleanup(func() { _ = c.Close() })
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not connect to server")
	}

	cancel()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected RunProxy error on cancel: %v", err)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("RunProxy hung after context cancellation with blocked stdin pipe")
	}

}

func TestRunProxy_ClientStdinEOFFlushesResponse(t *testing.T) {
	stateDir := t.TempDir()
	ln, err := listenUnix(stateDir)
	if err != nil {
		t.Fatalf("listenUnix: %v", err)
	}
	defer ln.Close()
	defer os.Remove(SocketPath(stateDir))

	serverErr := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer c.Close()
		req, err := io.ReadAll(c)
		if err != nil {
			serverErr <- err
			return
		}
		_, err = c.Write(append([]byte("echo:"), req...))
		serverErr <- err
	}()

	pr, pw := io.Pipe()
	var out bytes.Buffer

	done := make(chan error, 1)
	go func() {
		done <- RunProxy(context.Background(), stateDir, pr, &out)
	}()

	if _, err := pw.Write([]byte("hello")); err != nil {
		t.Fatalf("pipe write: %v", err)
	}
	if err := pw.Close(); err != nil {
		t.Fatalf("pipe close: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected RunProxy to return nil on stdin EOF, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunProxy hung after client stdin EOF")
	}

	if err := <-serverErr; err != nil {
		t.Fatalf("server error: %v", err)
	}
	if got := out.String(); got != "echo:hello" {
		t.Fatalf("expected flushed response %q, got %q", "echo:hello", got)
	}
}

func TestRunProxy_ReportsUnexpectedLeaderDrop(t *testing.T) {
	stateDir := t.TempDir()
	ln, err := listenUnix(stateDir)
	if err != nil {
		t.Fatalf("listenUnix: %v", err)
	}
	defer ln.Close()
	defer os.Remove(SocketPath(stateDir))

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	pr, pw := io.Pipe()
	t.Cleanup(func() {
		_ = pw.Close()
		_ = pr.Close()
	})
	var out bytes.Buffer

	done := make(chan error, 1)
	go func() {
		done <- RunProxy(context.Background(), stateDir, pr, &out)
	}()

	var serverConn net.Conn
	select {
	case serverConn = <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not connect to server")
	}

	// Simulate unexpected leader drop while stdin remains open.
	_ = serverConn.Close()

	select {
	case err := <-done:
		if !errors.Is(err, ErrLeaderDisconnected) {
			t.Fatalf("expected leader disconnect error, got %v", err)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("RunProxy hung after unexpected leader drop")
	}
}
