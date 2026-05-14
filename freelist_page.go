package frostfire

import (
	"encoding/binary"
	"fmt"
)

// Freelist is presisted as a singly-linked chain of pages. Meta.freelistRoot
// points at the first page, and each page carries up to freelistPerPage page
// ids that may be reused by future write transactions.
//
//	+-----------+-----------+-----------+----------------------------+
//	| bytes 0-7 | bytes 8-11| bytes12-15| bytes 16..PageSize-1       |
//	| next page | id count  | padding   | count little-endian PageId |
//	+-----------+-----------+-----------+----------------------------+
//
// next page is 0 at the end of the chain.

const (
	freelistPageHeader = 16 // 8B nextPageId + 4B count + 4B pad
	freelistPerPage    = (PageSize - freelistPageHeader) / 8
)

func encodeFreelistPage(buf []byte, ids []PageId, next PageId) {
	if len(ids) > freelistPerPage {
		panic("encodeFreelistPage: too many ids")
	}
	binary.LittleEndian.PutUint64(buf[0:8], uint64(next))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(len(ids)))
	binary.LittleEndian.PutUint32(buf[12:16], 0)
	for i, id := range ids {
		off := freelistPageHeader + i*8
		binary.LittleEndian.PutUint64(buf[off:off+8], uint64(id))
	}
}

func freelistPageNext(buf []byte) PageId {
	return PageId(binary.LittleEndian.Uint64(buf[0:8]))
}

func decodeFreelistPage(buf []byte) (ids []PageId, next PageId, err error) {
	if len(buf) < freelistPageHeader {
		return nil, 0, fmt.Errorf("freelist page: buf too small: %d", len(buf))
	}
	next = PageId(binary.LittleEndian.Uint64(buf[0:8]))
	count := binary.LittleEndian.Uint32(buf[8:12])
	if int(count) > freelistPerPage {
		return nil, 0, fmt.Errorf("freelist page: bad count %d", count)
	}
	ids = make([]PageId, count)
	for i := range int(count) {
		off := freelistPageHeader + i*8
		ids[i] = PageId(binary.LittleEndian.Uint64(buf[off : off+8]))
	}
	return ids, next, nil
}
