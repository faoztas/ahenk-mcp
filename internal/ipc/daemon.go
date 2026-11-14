// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package ipc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	active int64
}

// StartDaemon removes any stale socket, binds a new one with 0600 permissions,
// and starts accepting concurrent client connections.
func StartDaemon(ctx context.Context, stateDir string, server *mcp.Server) (*Daemon, error) {
	sockPath := SocketPath(stateDir)
	// Stale socket cleanup.
	_ = os.Remove(sockPath)

	ln, err := listenUnix(stateDir)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sockPath, 0600); err != nil {
		ln.Close()
		return nil, err
	}

	dctx, cancel := context.WithCancel(ctx)
	d := &Daemon{
		listener: ln,
		server:   server,
		ctx:      dctx,
		cancel:   cancel,
	}
	d.wg.Go(d.acceptLoop)
	return d, nil
}

func (d *Daemon) acceptLoop() {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			if d.ctx.Err() != nil {
				return
			}
			continue
		}
		atomic.AddInt64(&d.active, 1)
		d.wg.Go(func() {
			defer atomic.AddInt64(&d.active, -1)
			defer conn.Close()
			transport := &mcp.IOTransport{
				Reader:        conn,
				Writer:        conn,
				MaxLineLength: 512 << 10,
			}
			session, err := d.server.Connect(d.ctx, transport, nil)
			if err != nil {
				return
			}
			session.Wait()
		})
	}
}

// ActiveClients returns the number of currently connected socket clients.
func (d *Daemon) ActiveClients() int {
	return int(atomic.LoadInt64(&d.active))
}

// Close stops the daemon and removes the socket file.
func (d *Daemon) Close() error {
	d.cancel()
	err := d.listener.Close()
	d.wg.Wait()
	sockPath := d.listener.Addr().String()
	_ = os.Remove(sockPath)
	return err
}

var chdirMu sync.Mutex

func listenUnix(stateDir string) (net.Listener, error) {
	chdirMu.Lock()
	defer chdirMu.Unlock()
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if err := os.Chdir(stateDir); err != nil {
		return nil, err
	}
	defer os.Chdir(wd)
	return net.Listen("unix", "ahenk.sock")
}

func dialUnix(stateDir string, timeout time.Duration) (net.Conn, error) {
	chdirMu.Lock()
	defer chdirMu.Unlock()
	wd, _ := os.Getwd()
	if err := os.Chdir(stateDir); err != nil {
		return nil, err
	}
	defer os.Chdir(wd)
	return net.DialTimeout("unix", "ahenk.sock", timeout)
}
