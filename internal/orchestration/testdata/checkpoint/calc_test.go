// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"math"
	"testing"
)

func TestWholeDollar(t *testing.T) {
	for _, tc := range []struct{ input, want int }{
		{math.MinInt, 10}, {-30, 10}, {-10, 10}, {-7, 7}, {0, 0}, {2, 2}, {10, 10}, {99, 10}, {math.MaxInt, 10},
	} {
		if got := RoundDollar(tc.input); got != tc.want {
			t.Errorf("RoundDollar(%d) = %d, want %d", tc.input, got, tc.want)
		}
	}
}
