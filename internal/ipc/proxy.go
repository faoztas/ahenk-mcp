// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package ipc

import (
	"context"
	"io"
	"net"
	"sync"
	"time"
)

// RunProxy connects to the leader socket and bridges stdin <-> unixConn <-> stdout.
// An optional failover hook is invoked when the socket connection is lost.
func RunProxy(ctx context.Context, stateDir string, in io.Reader, out io.Writer, failover ...func() error) error {
	// Dial with backoff (up to 2s).
	deadline := time.Now().Add(2 * time.Second)
	var conn net.Conn
	var err error
	for {
		conn, err = dialUnix(stateDir, 500*time.Millisecond)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err != nil {
		if len(failover) > 0 && failover[0] != nil {
			return failover[0]()
		}
		return err
	}
	defer conn.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(conn, in)
		if uc, ok := conn.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		} else {
			_ = conn.Close()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(out, conn)
	}()
	wg.Wait()

	if len(failover) > 0 && failover[0] != nil && ctx.Err() == nil {
		return failover[0]()
	}
	return nil
}
