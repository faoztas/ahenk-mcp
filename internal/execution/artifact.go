// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"io"
	"os"
	"syscall"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func ReadVerifiedDiff(dir string, result *model.Result, id string, limit int64) ([]byte, error) {
	unavailable := model.Error("NOT_FOUND", "artifact unavailable or invalid")
	if result == nil || id != result.ArtifactID || !model.ValidID(id) {
		return nil, unavailable
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, unavailable
	}
	defer root.Close()
	f, err := root.OpenFile(id+".diff", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, unavailable
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > limit {
		return nil, unavailable
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit || model.Hash(data) != result.ArtifactSHA256 {
		return nil, unavailable
	}
	return data, nil
}
