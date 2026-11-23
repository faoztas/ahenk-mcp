// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package ipc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SocketPath returns the Unix domain socket path for a state directory.
func SocketPath(dir string) string {
	return filepath.Join(dir, "ahenk.sock")
}

// Daemon serves multiple MCP clients over a Unix domain socket using a single
// shared *mcp.Server.
type Daemon struct {
	listener net.Listener
	server   *mcp.Server
	sockPath string
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	idleCh chan struct{}
}

// StartDaemon removes any stale socket, binds a new one with 0600 permissions,
// and starts accepting concurrent client connections.
func StartDaemon(ctx context.Context, stateDir string, server *mcp.Server) (*Daemon, error) {
	sockPath := SocketPath(stateDir)
	if abs, err := filepath.Abs(sockPath); err == nil {
		sockPath = abs
	}
	// Stale socket cleanup.
	_ = os.Remove(sockPath)

	ln, err := listenUnix(stateDir)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sockPath, 0600); err != nil {
		_ = ln.Close()
		_ = os.Remove(sockPath)
		return nil, err
	}

	dctx, cancel := context.WithCancel(ctx)
	d := &Daemon{
		listener: ln,
		server:   server,
		sockPath: sockPath,
		ctx:      dctx,
		cancel:   cancel,
		conns:    make(map[net.Conn]struct{}),
	}
	d.wg.Go(d.acceptLoop)
	return d, nil
}

func (d *Daemon) acceptLoop() {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			d.mu.Lock()
			closed := d.closed
			d.mu.Unlock()
			if d.ctx.Err() != nil || closed {
				return
			}
			continue
		}
		d.mu.Lock()
		if d.closed {
			d.mu.Unlock()
			_ = conn.Close()
			return
		}
		if d.idleCh == nil {
			d.idleCh = make(chan struct{})
		}
		d.conns[conn] = struct{}{}
		d.mu.Unlock()

		d.wg.Go(func() {
			defer func() {
				d.mu.Lock()
				delete(d.conns, conn)
				if len(d.conns) == 0 && d.idleCh != nil {
					close(d.idleCh)
					d.idleCh = nil
				}
				d.mu.Unlock()
				_ = conn.Close()
			}()
			transport := &mcp.IOTransport{
				Reader:        conn,
				Writer:        conn,
				MaxLineLength: 512 << 10,
			}
			session, err := d.server.Connect(d.ctx, transport, nil)
			if err != nil {
				return
			}
			_ = session.Wait()
		})
	}
}

// ActiveClients returns the number of currently connected socket clients.
func (d *Daemon) ActiveClients() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.conns)
}

// WaitIdle blocks until all active socket clients disconnect or ctx is canceled.
func (d *Daemon) WaitIdle(ctx context.Context) error {
	for {
		d.mu.Lock()
		if len(d.conns) == 0 || d.closed {
			d.mu.Unlock()
			return nil
		}
		ch := d.idleCh
		d.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Close stops the daemon, terminates active connections, and removes the socket file.
func (d *Daemon) Close() error {
	d.cancel()
	d.mu.Lock()
	d.closed = true
	for conn := range d.conns {
		_ = conn.Close()
	}
	d.mu.Unlock()
	err := d.listener.Close()
	d.wg.Wait()
	if d.sockPath != "" {
		_ = os.Remove(d.sockPath)
	}
	return err
}

var chdirMu sync.Mutex

func listenUnix(stateDir string) (ln net.Listener, err error) {
	chdirMu.Lock()
	defer chdirMu.Unlock()
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if err := os.Chdir(stateDir); err != nil {
		return nil, err
	}
	defer func() {
		if chErr := os.Chdir(wd); chErr != nil && err == nil {
			if ln != nil {
				_ = ln.Close()
				ln = nil
			}
			err = chErr
		}
	}()
	return net.Listen("unix", "ahenk.sock")
}

func dialUnix(stateDir string, timeout time.Duration) (conn net.Conn, err error) {
	chdirMu.Lock()
	defer chdirMu.Unlock()
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if err := os.Chdir(stateDir); err != nil {
		return nil, err
	}
	defer func() {
		if chErr := os.Chdir(wd); chErr != nil && err == nil {
			if conn != nil {
				_ = conn.Close()
				conn = nil
			}
			err = chErr
		}
	}()
	return net.DialTimeout("unix", "ahenk.sock", timeout)
}
