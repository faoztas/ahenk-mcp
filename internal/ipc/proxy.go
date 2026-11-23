// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package ipc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"
)

var ErrLeaderDisconnected = errors.New("IPC leader disconnected. Reconnect to start a new MCP session")

// RunProxy relays one MCP session to the leader and owns the input stream.
// If the leader disappears, a new connection must negotiate its own session.
func RunProxy(ctx context.Context, stateDir string, in io.ReadCloser, out io.Writer) error {
	defer in.Close()

	deadline := time.Now().Add(2 * time.Second)
	var conn net.Conn
	var err error
	for {
		conn, err = dialUnix(stateDir, 500*time.Millisecond)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: %v", ErrLeaderDisconnected, err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	var clientEOF atomic.Bool
	go func() {
		if _, err := io.Copy(conn, in); err != nil {
			_ = conn.Close()
			return
		}
		clientEOF.Store(true)
		if unixConn, ok := conn.(*net.UnixConn); ok {
			_ = unixConn.CloseWrite()
		} else {
			_ = conn.Close()
		}
	}()

	_, err = io.Copy(out, conn)
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrLeaderDisconnected, err)
	}
	if !clientEOF.Load() {
		return ErrLeaderDisconnected
	}
	return nil
}
