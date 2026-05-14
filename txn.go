package frostfire

import "errors"

var (
	ErrTxnReadOnly = errors.New("frostfire: write operation on read-only transaction")
	ErrTxnClosed   = errors.New("frostfire: transaction already closed")
)

type TxnID uint64

type Txn struct {
	db       *DB
	writable bool
	closed   bool

	meta           Meta
	initialPageCnt uint64

	allocated []PageId // pages this txn allocated
	freed     []PageId // pre-existing pages this txn freed; gated under newTxnID
	reusable  []PageId // pages allocated and freed within this txn; safe for immediate reuse

	buckets map[string]*Bucket
}

func (db *DB) BeginRead() *Txn {
	db.readersMu.Lock()
	m := db.currentMeta.Load()
	db.readers[m.txnID]++
	db.readersMu.Unlock()
	return &Txn{
		db:             db,
		writable:       false,
		meta:           *m,
		initialPageCnt: m.numPages,
	}
}

func (db *DB) BeginWrite() *Txn {
	db.writerMu.Lock()
	m := db.currentMeta.Load()
	return &Txn{
		db:             db,
		writable:       true,
		meta:           *m,
		initialPageCnt: m.numPages,
	}
}

func (t *Txn) CatalogRoot() PageId { return t.meta.catalogRoot }

func (t *Txn) SetCatalogRoot(id PageId) error {
	if !t.writable {
		return ErrTxnReadOnly
	}
	if t.closed {
		return ErrTxnClosed
	}
	t.meta.catalogRoot = id
	return nil
}

func (t *Txn) Get(id PageId) (*Page, error) {
	if t.closed {
		return nil, ErrTxnClosed
	}
	return t.db.bufferPool.Get(id)
}

func (t *Txn) AllocatePage() (*Page, error) {
	if !t.writable {
		return nil, ErrTxnReadOnly
	}
	if t.closed {
		return nil, ErrTxnClosed
	}

	// 1. Reuse a page allocated and then freed within this same txn.
	if n := len(t.reusable); n > 0 {
		id := t.reusable[n-1]
		t.reusable = t.reusable[:n-1]
		page, err := t.db.bufferPool.GetForWrite(id)
		if err != nil {
			return nil, err
		}
		clear(page.data)
		t.allocated = append(t.allocated, id)
		return page, nil
	}

	// 2. Pop from the in-memory freelist if the head is gated below
	//    minReaderTxn.
	if id, ok := t.db.freelist.popReusable(t.db.minReaderTxn()); ok {
		page, err := t.db.bufferPool.GetForWrite(id)
		if err != nil {
			return nil, err
		}
		clear(page.data)
		t.allocated = append(t.allocated, id)
		return page, nil
	}

	// 3. Extend the file.
	id := PageId(t.meta.numPages)
	t.meta.numPages++
	page, err := t.db.bufferPool.AllocatePage(id)
	if err != nil {
		return nil, err
	}
	t.allocated = append(t.allocated, id)
	return page, nil
}

func (t *Txn) FreePage(id PageId) {
	if !t.writable {
		panic("Txn.FreePage: read-only transaction")
	}
	if t.closed {
		panic("Txn.FreePage: transaction closed")
	}
	if uint64(id) >= t.initialPageCnt {
		// Allocated and freed in this same txn.
		for i, a := range t.allocated {
			if a == id {
				t.allocated = append(t.allocated[:i], t.allocated[i+1:]...)
				break
			}
		}
		t.reusable = append(t.reusable, id)
		return
	}
	t.freed = append(t.freed, id)
}

func (t *Txn) Commit() error {
	if !t.writable {
		return ErrTxnReadOnly
	}
	if t.closed {
		return ErrTxnClosed
	}

	if err := t.applyBucketChanges(); err != nil {
		t.abortInternal()
		return err
	}

	if err := t.persistFreelist(); err != nil {
		t.abortInternal()
		return err
	}

	if err := t.db.bufferPool.FlushPages(t.allocated); err != nil {
		t.abortInternal()
		return err
	}
	if err := t.db.bufferPool.Sync(); err != nil {
		t.abortInternal()
		return err
	}
	t.meta.txnID++
	if err := t.db.commitMeta(&t.meta); err != nil {
		t.abortInternal()
		return err
	}

	t.db.freelist.push(t.meta.txnID, t.freed...)
	t.db.freelist.push(t.meta.txnID, t.reusable...)

	t.closed = true
	t.db.writerMu.Unlock()
	return nil
}

func (t *Txn) abortInternal() {
	for _, id := range t.allocated {
		t.db.bufferPool.MarkClean(id)
	}
	for _, id := range t.reusable {
		t.db.bufferPool.MarkClean(id)
	}
	t.closed = true
	t.db.writerMu.Unlock()
}

func (t *Txn) Abort() {
	if t.closed {
		return
	}
	if t.writable {
		for _, id := range t.allocated {
			t.db.bufferPool.MarkClean(id)
		}
		for _, id := range t.reusable {
			t.db.bufferPool.MarkClean(id)
		}
	} else {
		t.db.unregisterReader(t.meta.txnID)
	}
	t.closed = true
	if t.writable {
		t.db.writerMu.Unlock()
	}
}
