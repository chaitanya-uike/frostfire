package frostfire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc64"
)

const (
	metaMagic    uint32 = 0x46495245 // "FIRE"
	metaVersion  uint32 = 1
	metaPage0    PageId = 0
	metaPage1    PageId = 1
	metaSize            = 56
	metaChkStart        = 48
)

var (
	ErrMetaCorrupt = errors.New("frostfire: meta pages corrupt")
	crcTable       = crc64.MakeTable(crc64.ISO)
)

// Meta is the main database header. Stored in two
// alternating pages (0 and 1); the newer one (by txnID) wins on load.
type Meta struct {
	magic        uint32
	version      uint32
	pageSize     uint32
	txnID        TxnID
	catalogRoot  PageId
	freelistRoot PageId
	numPages     uint64
	checksum     uint64
}

func (m *Meta) encode(buf []byte) error {
	if len(buf) < metaSize {
		return fmt.Errorf("meta encode: buffer too small: %d", len(buf))
	}
	binary.LittleEndian.PutUint32(buf[0:4], m.magic)
	binary.LittleEndian.PutUint32(buf[4:8], m.version)
	binary.LittleEndian.PutUint32(buf[8:12], m.pageSize)
	binary.LittleEndian.PutUint32(buf[12:16], 0) // pad
	binary.LittleEndian.PutUint64(buf[16:24], uint64(m.txnID))
	binary.LittleEndian.PutUint64(buf[24:32], uint64(m.catalogRoot))
	binary.LittleEndian.PutUint64(buf[32:40], uint64(m.freelistRoot))
	binary.LittleEndian.PutUint64(buf[40:48], m.numPages)
	m.checksum = crc64.Checksum(buf[:metaChkStart], crcTable)
	binary.LittleEndian.PutUint64(buf[48:56], m.checksum)
	return nil
}

func decodeMeta(buf []byte) (*Meta, error) {
	if len(buf) < metaSize {
		return nil, fmt.Errorf("meta decode: buf too small: %d", len(buf))
	}
	got := binary.LittleEndian.Uint64(buf[48:56])
	want := crc64.Checksum(buf[:metaChkStart], crcTable)
	if got != want {
		return nil, fmt.Errorf("checksum mismatch: got %x want %x", got, want)
	}
	m := &Meta{
		magic:        binary.LittleEndian.Uint32(buf[0:4]),
		version:      binary.LittleEndian.Uint32(buf[4:8]),
		pageSize:     binary.LittleEndian.Uint32(buf[8:12]),
		txnID:        TxnID(binary.LittleEndian.Uint64(buf[16:24])),
		catalogRoot:  PageId(binary.LittleEndian.Uint64(buf[24:32])),
		freelistRoot: PageId(binary.LittleEndian.Uint64(buf[32:40])),
		numPages:     binary.LittleEndian.Uint64(buf[40:48]),
		checksum:     want,
	}
	if m.magic != metaMagic {
		return nil, fmt.Errorf("bad magic: got %x want %x", m.magic, metaMagic)
	}
	if m.version != metaVersion {
		return nil, fmt.Errorf("unsupported version: %d", m.version)
	}
	if m.pageSize != PageSize {
		return nil, fmt.Errorf("page size mismatch: got %d want %d", m.pageSize, PageSize)
	}
	return m, nil
}

func isNewer(a, b TxnID) bool {
	// should be safe in case of overflow
	return int64(a-b) > 0
}

func (db *DB) commitMeta(newMeta *Meta) error {
	nextPage := db.nextMetaPage()

	if err := db.writeMeta(newMeta, nextPage); err != nil {
		return fmt.Errorf("commitMeta: write meta: %w", err)
	}
	if err := db.bufferPool.FlushPage(nextPage); err != nil {
		return fmt.Errorf("commitMeta: flush meta page: %w", err)
	}
	if err := db.bufferPool.Sync(); err != nil {
		return fmt.Errorf("commitMeta: final sync: %w", err)
	}

	db.currentMeta.Store(newMeta)
	db.currentMetaPage = nextPage
	return nil
}

func (db *DB) loadMeta() (*Meta, PageId, error) {
	page0, err0 := db.bufferPool.Get(metaPage0)
	if err0 == nil {
		defer page0.Unpin()
	}
	page1, err1 := db.bufferPool.Get(metaPage1)
	if err1 == nil {
		defer page1.Unpin()
	}

	var m0, m1 *Meta
	var d0, d1 error

	if err0 == nil {
		m0, d0 = decodeMeta(page0.data)
	} else {
		d0 = err0
	}
	if err1 == nil {
		m1, d1 = decodeMeta(page1.data)
	} else {
		d1 = err1
	}

	switch {
	case d0 == nil && d1 == nil:
		if isNewer(m1.txnID, m0.txnID) {
			return m1, metaPage1, nil
		}
		return m0, metaPage0, nil
	case d0 == nil:
		return m0, metaPage0, nil
	case d1 == nil:
		return m1, metaPage1, nil
	default:
		return nil, 0, fmt.Errorf("%w: page0=%v page1=%v", ErrMetaCorrupt, d0, d1)
	}
}

func (db *DB) nextMetaPage() PageId {
	if db.currentMetaPage == metaPage0 {
		return metaPage1
	}
	return metaPage0
}

func (db *DB) writeMeta(m *Meta, id PageId) error {
	page, err := db.bufferPool.GetForWrite(id)
	if err != nil {
		return err
	}
	defer page.Unpin()
	return m.encode(page.data)
}

func (db *DB) initMetas() error {
	base := &Meta{
		magic:        metaMagic,
		version:      metaVersion,
		pageSize:     PageSize,
		catalogRoot:  0,
		freelistRoot: 0,
		numPages:     2,
		txnID:        0,
	}

	page0, err := db.bufferPool.AllocatePage(metaPage0)
	if err != nil {
		return err
	}
	if err := base.encode(page0.data); err != nil {
		page0.Unpin()
		return err
	}
	page0.Unpin()

	page1, err := db.bufferPool.AllocatePage(metaPage1)
	if err != nil {
		return err
	}
	if err := base.encode(page1.data); err != nil {
		page1.Unpin()
		return err
	}
	page1.Unpin()

	if err := db.bufferPool.FlushAll(); err != nil {
		return err
	}
	if err := db.bufferPool.Sync(); err != nil {
		return err
	}

	db.currentMeta.Store(base)
	db.currentMetaPage = metaPage0
	return nil
}
