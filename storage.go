package frostfire

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

var (
	ErrInvalidFileSize = errors.New("file size not a multiple of page size")
)

type PageId uint64

const (
	PageSize     = 4096
	initFileSize = 16 * PageSize
	maxGrowth    = 256 * PageSize //allocate doubles the size till this cap
)

type StorageManager struct {
	file     *os.File
	fileSize int64
}

func NewStorageManager(path string) (*StorageManager, bool, error) {
	file, err := openFileDirect(path)
	if err != nil {
		return nil, false, err
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, false, err
	}

	size := info.Size()
	if size%PageSize != 0 {
		file.Close()
		return nil, false, ErrInvalidFileSize
	}

	return &StorageManager{
		file:     file,
		fileSize: size,
	}, size == 0, nil
}

func (s *StorageManager) ReadPage(pageId PageId, buf []byte) error {
	_, err := s.file.ReadAt(buf, s.getOffset(pageId))
	return err
}

func (s *StorageManager) WritePage(pageId PageId, buf []byte) error {
	_, err := s.file.WriteAt(buf, s.getOffset(pageId))
	return err
}

const pwritevMaxIOV = 1024

func (s *StorageManager) WritePagesContiguous(startPageID PageId, bufs [][]byte) error {
	if len(bufs) == 0 {
		return nil
	}
	offset := s.getOffset(startPageID)
	if offset+int64(len(bufs))*PageSize > s.fileSize {
		panic("out of bounds page access")
	}
	return pwritevFull(s.file, bufs, offset)
}

func pwritevFull(f *os.File, bufs [][]byte, offset int64) error {
	for len(bufs) > 0 {
		chunk := bufs
		if len(chunk) > pwritevMaxIOV {
			chunk = bufs[:pwritevMaxIOV]
		}
		n, err := unix.Pwritev(int(f.Fd()), chunk, offset)
		if err != nil {
			return err
		}
		full := int(int64(n) / PageSize)
		partial := int64(n) % PageSize
		bufs = bufs[full:]
		offset += int64(full) * PageSize
		if partial > 0 {
			bufs[0] = bufs[0][partial:]
			offset += partial
		}
	}
	return nil
}

func (s *StorageManager) Close() error {
	return s.file.Close()
}

func (s *StorageManager) Sync() error {
	return fsyncFile(s.file)
}

func (s *StorageManager) Allocate(pageId PageId) error {
	required := (int64(pageId) + 1) * PageSize
	if required <= s.fileSize {
		return nil
	}
	newSize := s.growSize(required)
	if err := s.file.Truncate(newSize); err != nil {
		return err
	}
	s.fileSize = newSize
	return nil
}

func (s *StorageManager) growSize(required int64) int64 {
	size := max(s.fileSize, initFileSize)
	for size < required {
		growth := max(size, maxGrowth)
		size += growth
	}
	return size
}

func (s *StorageManager) getOffset(pageId PageId) int64 {
	offset := int64(pageId) * PageSize
	if offset+PageSize > s.fileSize {
		panic("out of bounds page access")
	}
	return offset
}

func syncDir(dir string) error {
	dirFd, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer dirFd.Close()
	return dirFd.Sync()
}
