package frostfire

import "encoding/binary"

const (
	overflowHeaderSize = 9
	overflowChunkSize  = PageSize - overflowHeaderSize
)

func (t *BTree) writeOverflowPages(value []byte) (PageId, error) {
	var firstID PageId
	var prev *Page
	havePrev := false
	for off := 0; off < len(value); off += overflowChunkSize {
		page, err := t.txn.AllocatePage()
		if err != nil {
			if havePrev {
				prev.Unpin()
			}
			return 0, err
		}
		page.data[0] = nodeOverflow
		binary.LittleEndian.PutUint64(page.data[1:9], 0)

		end := min(off+overflowChunkSize, len(value))
		copy(page.data[overflowHeaderSize:], value[off:end])

		if havePrev {
			binary.LittleEndian.PutUint64(prev.data[1:9], uint64(page.pageId))
			prev.Unpin()
		} else {
			firstID = page.pageId
		}
		prev = page
		havePrev = true
	}
	if havePrev {
		prev.Unpin()
	}
	return firstID, nil
}

func (t *BTree) readOverflowPages(firstID PageId, vallen int) ([]byte, error) {
	value := make([]byte, vallen)
	off := 0
	id := firstID
	for id != 0 {
		page, err := t.txn.Get(id)
		if err != nil {
			return nil, err
		}
		remaining := vallen - off
		chunk := min(overflowChunkSize, remaining)
		copy(value[off:], page.data[overflowHeaderSize:overflowHeaderSize+chunk])
		off += chunk
		next := PageId(binary.LittleEndian.Uint64(page.data[1:9]))
		page.Unpin()
		id = next
	}
	return value, nil
}

func (t *BTree) freeOverflowPages(firstID PageId) error {
	id := firstID
	for id != 0 {
		page, err := t.txn.Get(id)
		if err != nil {
			return err
		}
		next := PageId(binary.LittleEndian.Uint64(page.data[1:9]))
		page.Unpin()
		t.txn.FreePage(id)
		id = next
	}
	return nil
}

func (t *BTree) leafCellOverflow(key, value []byte) (PageId, error) {
	if leafCellHeader+len(key)+len(value) <= maxInlineSize {
		return 0, nil
	}
	return t.writeOverflowPages(value)
}
