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
	if model.ContainsSecret([]byte(brief.Objective)) {
		return errors.New("private brief objective contains a secret pattern")
	}
	if model.ContainsPersonalData([]byte(brief.Objective)) {
		return errors.New("private brief objective contains classified personal data")
	}
	ids := make(map[string]bool, len(brief.Acceptance))
	for _, c := range brief.Acceptance {
		if !model.ValidID(c.ID) || ids[c.ID] || len(c.Description) < 1 || len(c.Description) > 2048 ||
			(c.CheckID != "" && !model.ValidID(c.CheckID)) {
			return errors.New("invalid acceptance criterion in private brief")
		}
		if model.ContainsSecret([]byte(c.Description)) {
			return errors.New("private brief acceptance criterion contains a secret pattern")
		}
		if model.ContainsPersonalData([]byte(c.Description)) {
			return errors.New("private brief acceptance criterion contains classified personal data")
		}
		ids[c.ID] = true
	}
	fmt.Println(model.PrivateBriefDigest(brief.Objective, brief.Acceptance))
	return nil
}
