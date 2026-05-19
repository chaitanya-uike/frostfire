package frostfire

import (
	"encoding/binary"
	"errors"
)

var (
	ErrBucketNotFound = errors.New("frostfire: bucket not found")
	ErrBucketExists   = errors.New("frostfire: bucket already exists")
)

type Bucket struct {
	txn      *Txn
	name     []byte
	bt       *BTree
	origRoot PageId
	created  bool
	dropped  bool
}

func (b *Bucket) Get(key []byte) ([]byte, error) {
	if b.dropped {
		return nil, ErrBucketNotFound
	}
	return b.bt.Search(key)
}

func (b *Bucket) Put(key, value []byte) error {
	if b.dropped {
		return ErrBucketNotFound
	}
	_, err := b.bt.Update(key, value, ModeUpsert)
	return err
}

func (b *Bucket) Insert(key, value []byte) error {
	if b.dropped {
		return ErrBucketNotFound
	}
	_, err := b.bt.Update(key, value, ModeInsert)
	return err
}

func (b *Bucket) Update(key, value []byte) error {
	if b.dropped {
		return ErrBucketNotFound
	}
	_, err := b.bt.Update(key, value, ModeUpdate)
	return err
}

func (b *Bucket) Delete(key []byte) error {
	if b.dropped {
		return ErrBucketNotFound
	}
	_, _, err := b.bt.Delete(key)
	return err
}

func (b *Bucket) BeginBulkLoad(opts BulkLoadOptions) (*BulkLoader, error) {
	if b.dropped {
		return nil, ErrBucketNotFound
	}
	return b.bt.BeginBulkLoad(opts)
}

func (t *Txn) Bucket(name []byte) (*Bucket, error) {
	if t.closed {
		return nil, ErrTxnClosed
	}
	return t.lookupBucket(name)
}

func (t *Txn) CreateBucket(name []byte) (*Bucket, error) {
	if !t.writable {
		return nil, ErrTxnReadOnly
	}
	if t.closed {
		return nil, ErrTxnClosed
	}
	if t.buckets == nil {
		t.buckets = make(map[string]*Bucket)
	}
	key := string(name)
	if existing, ok := t.buckets[key]; ok {
		if !existing.dropped {
			return nil, ErrBucketExists
		}
		// recreating after drop in the same txn
		existing.bt = NewBTree(t, 0)
		existing.dropped = false
		existing.created = true
		return existing, nil
	}
	if _, found, err := t.catalogLookup(name); err != nil {
		return nil, err
	} else if found {
		return nil, ErrBucketExists
	}
	b := &Bucket{
		txn:      t,
		name:     append([]byte(nil), name...),
		bt:       NewBTree(t, 0),
		origRoot: 0,
		created:  true,
	}
	t.buckets[key] = b
	return b, nil
}

func (t *Txn) DropBucket(name []byte) error {
	if !t.writable {
		return ErrTxnReadOnly
	}
	if t.closed {
		return ErrTxnClosed
	}
	b, err := t.lookupBucket(name)
	if err != nil {
		return err
	}
	if err := b.bt.FreeAll(); err != nil {
		return err
	}
	b.bt = nil
	b.dropped = true
	return nil
}

func (t *Txn) lookupBucket(name []byte) (*Bucket, error) {
	if t.buckets == nil {
		t.buckets = make(map[string]*Bucket)
	}
	key := string(name)
	if b, ok := t.buckets[key]; ok {
		if b.dropped {
			return nil, ErrBucketNotFound
		}
		return b, nil
	}
	root, found, err := t.catalogLookup(name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrBucketNotFound
	}
	b := &Bucket{
		txn:      t,
		name:     append([]byte(nil), name...),
		bt:       NewBTree(t, root),
		origRoot: root,
	}
	t.buckets[key] = b
	return b, nil
}

func (t *Txn) catalogLookup(name []byte) (PageId, bool, error) {
	if t.meta.catalogRoot == 0 {
		return 0, false, nil
	}
	cat := NewBTree(t, t.meta.catalogRoot)
	val, err := cat.Search(name)
	if err != nil {
		return 0, false, err
	}
	if val == nil {
		return 0, false, nil
	}
	if len(val) != 8 {
		return 0, false, errors.New("frostfire: malformed catalog entry")
	}
	return PageId(binary.LittleEndian.Uint64(val)), true, nil
}

func (t *Txn) applyBucketChanges() error {
	if len(t.buckets) == 0 {
		return nil
	}
	cat := NewBTree(t, t.meta.catalogRoot)
	var encoded [8]byte
	for name, b := range t.buckets {
		switch {
		case b.dropped:
			if b.origRoot == 0 {
				continue // created and dropped in same txn
			}
			if _, _, err := cat.Delete([]byte(name)); err != nil {
				return err
			}
		case b.created || b.bt.Root() != b.origRoot:
			binary.LittleEndian.PutUint64(encoded[:], uint64(b.bt.Root()))
			if _, err := cat.Update([]byte(name), encoded[:], ModeUpsert); err != nil {
				return err
			}
		}
	}
	t.meta.catalogRoot = cat.Root()
	return nil
}
