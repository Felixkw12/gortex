//go:build !darwin && !linux && !windows

package platform

import "errors"

func processBirth(_ int) ([2]uint64, error) {
	return [2]uint64{}, errors.New("native process birth identity unavailable on this platform")
}
