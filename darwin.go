//go:build darwin

package frostfire

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openFileDirect(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
	isNew := true
	if err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		isNew = false
		f, err = os.OpenFile(path, os.O_RDWR, 0o666)
		if err != nil {
			return nil, err
		}
	}
	// F_NOCACHE: disable data caching for this fd. non zero arg disables caching
	if _, err := unix.FcntlInt(f.Fd(), unix.F_NOCACHE, 1); err != nil {
		f.Close()
		return nil, err
	}

	if isNew {
		if err = syncDir(filepath.Dir(path)); err != nil {
			f.Close()
			return nil, err
		}
	}

	return f, nil
}

func NewBuffer() []byte {
	return make([]byte, PageSize)
}

func fsyncFile(f *os.File) error {
	_, err := unix.FcntlInt(f.Fd(), unix.F_FULLFSYNC, 0)
	return err
}
