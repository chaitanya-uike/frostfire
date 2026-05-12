//go:build linux

package frostfire

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
)

const AlignSize = 4096

func openFileDirect(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|unix.O_DIRECT, 0o666)
	isNew := true
	if err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		isNew = false
		f, err = os.OpenFile(path, os.O_RDWR|unix.O_DIRECT, 0o666)
		if err != nil {
			return nil, err
		}
	}
	if isNew {
		if err = syncDir(filepath.Dir(path)); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

func fsyncFile(f *os.File) error {
	return f.Sync()
}

func alignment(buf []byte) int {
	return int(uintptr(unsafe.Pointer(&buf[0])) & uintptr(AlignSize-1))
}

// for direct io buffers need to be block aligned
func NewBuffer() []byte {
	buf := make([]byte, PageSize+AlignSize)
	a := alignment(buf)
	offset := 0
	if a != 0 {
		offset = AlignSize - a
	}
	buf = buf[offset : offset+PageSize]
	return buf
}
