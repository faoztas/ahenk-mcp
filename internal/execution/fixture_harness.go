// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"encoding/json"
	"errors"
	"net"
)

// The supervised fixture speaks the worker wire protocol. Every operation
// and argument is fixed except for the controller-issued attempt identity.
func runFixtureHarness(endpoint *FixtureHarness) error {
	conn, err := net.Dial("unix", endpoint.SocketPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
	call := func(req map[string]string) (string, error) {
		req["attempt_id"] = endpoint.AttemptID
		if err := enc.Encode(req); err != nil {
			return "", err
		}
		var resp struct {
			Success bool
			Data    string
		}
		if err := dec.Decode(&resp); err != nil {
			return "", err
		}
		if !resp.Success {
			return "", errors.New("fixture harness operation denied")
		}
		return resp.Data, nil
	}
	original, err := call(map[string]string{"tool": "read_file", "path": "fixture.txt"})
	if err != nil {
		return err
	}
	if original != "before\n" {
		return errors.New("unexpected fixture preimage")
	}
	if _, err := call(map[string]string{"tool": "edit_file", "path": "fixture.txt", "old_content": original, "content": "after via harness\n"}); err != nil {
		return err
	}
	if _, err := call(map[string]string{"tool": "check", "check_id": "source-unchanged"}); err != nil {
		return err
	}
	_, err = call(map[string]string{"tool": "finish", "summary": "Fixture completed the harness edit."})
	return err
}
