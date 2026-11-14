// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

// ACPClient is a minimal stdio JSON-RPC 2.0 ACP client.
type ACPClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	nextID int
}

func NewACPClient(command string, args []string) *ACPClient {
	cmd := exec.Command(command, args...)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Start()
	return &ACPClient{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), nextID: 1}
}

func (c *ACPClient) call(method string, params any) (map[string]any, error) {
	id := c.nextID
	c.nextID++
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	b, _ := json.Marshal(req)
	c.stdin.Write(append(b, '\n'))
	// read until matching id
	for {
		line, err := c.stdout.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var resp map[string]any
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			continue
		}
		if v, ok := resp["id"].(float64); ok && int(v) == id {
			if e, ok := resp["error"]; ok {
				return nil, fmt.Errorf("rpc error: %v", e)
			}
			res, _ := resp["result"].(map[string]any)
			return res, nil
		}
		// notification handling ignored
	}
}

func (c *ACPClient) Initialize() error {
	res, err := c.call("initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": true, "writeTextFile": true}, "terminal": false},
	})
	if err != nil {
		return err
	}
	if res == nil {
		return model.Error("PROVIDER_STARTUP_FAILED", "no initialize result")
	}
	return nil
}

func (c *ACPClient) NewSession(cwd string) (string, error) {
	res, err := c.call("session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	if err != nil {
		return "", err
	}
	if res == nil {
		return "", model.Error("PROVIDER_STARTUP_FAILED", "no session")
	}
	sid, _ := res["sessionId"].(string)
	return sid, nil
}

func (c *ACPClient) Prompt(sessionID, prompt string) (string, error) {
	res, err := c.call("session/prompt", map[string]any{"sessionId": sessionID, "prompt": []map[string]any{{"type": "text", "text": prompt}}})
	if err != nil {
		return "", err
	}
	_ = res
	return "", nil
}

func (c *ACPClient) Close() error {
	if c.stdin != nil {
		c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		c.cmd.Process.Kill()
	}
	return nil
}
