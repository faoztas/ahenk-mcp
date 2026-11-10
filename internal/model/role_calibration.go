// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package model

type RoleCalibrationEvidence struct {
	Fixture        string   `json:"fixture"`
	Roles          []string `json:"roles"`
	ResponseSHA256 string   `json:"response_sha256"`
}
