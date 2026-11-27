//go:build !linux && !darwin

package traces

import "errors"

func ReadLocal(string) ([]byte, error) {
	return nil, errors.New("trace file import requires Linux or macOS")
}
