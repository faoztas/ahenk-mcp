// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func privateBriefDigest(args []string) error {
	if len(args) != 0 {
		return errors.New("private-brief-digest reads one reviewed brief from stdin")
	}
	var brief struct {
		Objective  string        `json:"objective"`
		Acceptance []model.Check `json:"acceptance"`
	}
	limit := &io.LimitedReader{R: os.Stdin, N: (64 << 10) + 1}
	decoder := json.NewDecoder(limit)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&brief) != nil || decoder.Decode(new(any)) != io.EOF || limit.N == 0 ||
		brief.Objective == "" || len(brief.Objective) > 8192 || len(brief.Acceptance) == 0 || len(brief.Acceptance) > 16 {
		return errors.New("invalid private brief")
	}
	fmt.Println(model.PrivateBriefDigest(brief.Objective, brief.Acceptance))
	return nil
}
