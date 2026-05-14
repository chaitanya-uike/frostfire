package frostfire

import (
	"sync"
	"sync/atomic"
)

const defaultBufferSize = 1024

type Options struct {
	BufferSize int // number of frames in the buffer pool; defaults to 1024 (4 MB at 4KB pages)
}

type DB struct {
	bufferPool *BufferPool

	currentMeta     atomic.Pointer[Meta]
	currentMetaPage PageId

	writerMu sync.Mutex

	freelist *freelist

	readersMu sync.Mutex
	readers   map[TxnID]int
}

func Open(path string, opts Options) (*DB, error) {
	smgr, isNew, err := NewStorageManager(path)
	if err != nil {
		return nil, err
	}
	bufSize := opts.BufferSize
	if bufSize == 0 {
		bufSize = defaultBufferSize
	}
	db := &DB{
		bufferPool: NewBufferPool(smgr, bufSize),
		freelist:   newFreelist(),
		readers:    make(map[TxnID]int),
	}
	if isNew {
		if err := db.initMetas(); err != nil {
			db.bufferPool.Close()
			return nil, err
		}
		return db, nil
	}

	m, page, err := db.loadMeta()
	if err != nil {
		db.bufferPool.Close()
		return nil, err
	}
	db.currentMeta.Store(m)
	db.currentMetaPage = page

	if m.freelistRoot != 0 {
		ids, err := db.readFreelistChain(m.freelistRoot)
		if err != nil {
			db.bufferPool.Close()
			return nil, err
		}
		db.freelist.load(ids)
	}
	return db, nil
}

func (db *DB) Close() error {
	return db.bufferPool.Close()
}

func (db *DB) unregisterReader(txnID TxnID) {
	db.readersMu.Lock()
	if db.readers[txnID]--; db.readers[txnID] <= 0 {
		delete(db.readers, txnID)
	}
	db.readersMu.Unlock()
}

func (db *DB) minReaderTxn() TxnID {
	current := db.currentMeta.Load().txnID
	db.readersMu.Lock()
	defer db.readersMu.Unlock()
	min := current
	for txnID := range db.readers {
		if txnID < min {
			min = txnID
		}
	}
	return min
}
