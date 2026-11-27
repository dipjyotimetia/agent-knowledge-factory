//go:build linux || darwin

package traces

import (
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

var errUnsafeFile = errors.New("trace import requires a readable regular file with no symlink components")

// ReadLocal walks literal components using directory descriptors. A symlink
// replacement fails at openat, and a replacement with a FIFO cannot block.
func ReadLocal(path string) ([]byte, error) {
	base := "."
	if strings.HasPrefix(path, "/") {
		base, path = "/", strings.TrimPrefix(path, "/")
	}
	parts := strings.Split(path, "/")
	dir, err := unix.Open(base, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errUnsafeFile
	}
	defer func() { _ = unix.Close(dir) }()
	for _, part := range parts[:len(parts)-1] {
		if part == ".." || part == "" {
			return nil, errUnsafeFile
		}
		nextDir, err := unix.Openat(dir, part, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, errUnsafeFile
		}
		_ = unix.Close(dir)
		dir = nextDir
	}
	name := parts[len(parts)-1]
	var initial unix.Stat_t
	if unix.Fstatat(dir, name, &initial, unix.AT_SYMLINK_NOFOLLOW) != nil || initial.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errUnsafeFile
	}
	if initial.Size > MaxBytes {
		return nil, ErrInvalid
	}
	fd, err := unix.Openat(dir, name, unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errUnsafeFile
	}
	file := os.NewFile(uintptr(fd), "trace input")
	defer file.Close()
	var opened unix.Stat_t
	if unix.Fstat(fd, &opened) != nil || opened.Mode&unix.S_IFMT != unix.S_IFREG ||
		opened.Dev != initial.Dev || opened.Ino != initial.Ino {
		return nil, errUnsafeFile
	}
	if opened.Size > MaxBytes {
		return nil, ErrInvalid
	}
	raw, err := io.ReadAll(io.LimitReader(file, MaxBytes+1))
	if err != nil {
		return nil, errUnsafeFile
	}
	if len(raw) > MaxBytes {
		return nil, ErrInvalid
	}
	return raw, nil
}
