package frostfire

import (
	"bytes"
	"errors"
	"testing"
)

func TestBulkLoadEmpty(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	bl, err := bt.BeginBulkLoad(BulkLoadOptions{})
	if err != nil {
		t.Fatalf("BeginBulkLoad: %v", err)
	}
	root, err := bl.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if root != 0 {
		t.Fatalf("expected root=0 after no appends, got %d", root)
	}
}

func TestBulkLoadSingleLeaf(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	bl, err := bt.BeginBulkLoad(BulkLoadOptions{})
	if err != nil {
		t.Fatalf("BeginBulkLoad: %v", err)
	}
	const n = 5
	for i := range n {
		if err := bl.Append(itob(i), smallVal(i)); err != nil {
			t.Fatalf("Append(%d): %v", i, err)
		}
	}
	if _, err := bl.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	for i := range n {
		got, err := bt.Search(itob(i))
		if err != nil {
			t.Fatalf("Search(%d): %v", i, err)
		}
		if !bytes.Equal(got, smallVal(i)) {
			t.Errorf("Search(%d) returned wrong value", i)
		}
	}
}

func TestBulkLoadManyKeys(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	bl, err := bt.BeginBulkLoad(BulkLoadOptions{})
	if err != nil {
		t.Fatalf("BeginBulkLoad: %v", err)
	}
	const n = 5000
	for i := range n {
		if err := bl.Append(itob(i), smallVal(i)); err != nil {
			t.Fatalf("Append(%d): %v", i, err)
		}
	}
	if _, err := bl.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	for i := range n {
		got, err := bt.Search(itob(i))
		if err != nil {
			t.Fatalf("Search(%d): %v", i, err)
		}
		if !bytes.Equal(got, smallVal(i)) {
			t.Errorf("Search(%d) returned wrong value", i)
		}
	}

	if _, err := bt.Search(itob(n + 100)); err != nil {
		t.Fatalf("Search miss: %v", err)
	}
}

func TestBulkLoadFillFactorHonored(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	bl, err := bt.BeginBulkLoad(BulkLoadOptions{FillFactor: 0.5})
	if err != nil {
		t.Fatalf("BeginBulkLoad: %v", err)
	}
	const n = 2000
	for i := range n {
		if err := bl.Append(itob(i), smallVal(i)); err != nil {
			t.Fatalf("Append(%d): %v", i, err)
		}
	}
	if _, err := bl.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	target := uint16(float64(PageSize) * 0.5)
	const slack = 512
	violations := 0
	var walk func(id PageId, rightmost bool)
	walk = func(id PageId, rightmost bool) {
		n, err := bt.get(id)
		if err != nil {
			t.Fatalf("get(%d): %v", id, err)
		}
		defer n.Unpin()
		if !rightmost {
			used := PageSize - n.freeSpace()
			if used > target+slack {
				violations++
			}
		}
		if n.ntype() == nodeLeaf {
			return
		}
		nCells := n.nCells()
		for i := range nCells + 1 {
			walk(n.child(i), rightmost && i == nCells)
		}
	}
	if bt.root != 0 {
		walk(bt.root, true)
	}
	if violations > 0 {
		t.Fatalf("%d sealed page(s) exceeded fill factor target+slack", violations)
	}
}

func TestBulkLoadRejectsNonEmpty(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	if _, err := bt.Update(itob(1), smallVal(1), ModeUpsert); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := bt.BeginBulkLoad(BulkLoadOptions{}); !errors.Is(err, ErrBTreeNotEmpty) {
		t.Fatalf("expected ErrBTreeNotEmpty, got %v", err)
	}
}

func TestBulkLoadRejectsOutOfOrder(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	bl, err := bt.BeginBulkLoad(BulkLoadOptions{})
	if err != nil {
		t.Fatalf("BeginBulkLoad: %v", err)
	}
	if err := bl.Append(itob(5), smallVal(5)); err != nil {
		t.Fatalf("Append(5): %v", err)
	}
	if err := bl.Append(itob(5), smallVal(5)); !errors.Is(err, ErrBulkLoaderKeyOrder) {
		t.Fatalf("equal key: expected ErrBulkLoaderKeyOrder, got %v", err)
	}
	if err := bl.Append(itob(3), smallVal(3)); !errors.Is(err, ErrBulkLoaderKeyOrder) {
		t.Fatalf("descending key: expected ErrBulkLoaderKeyOrder, got %v", err)
	}
	if _, err := bl.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

func TestBulkLoadRejectsAfterFinish(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	bl, err := bt.BeginBulkLoad(BulkLoadOptions{})
	if err != nil {
		t.Fatalf("BeginBulkLoad: %v", err)
	}
	if err := bl.Append(itob(1), smallVal(1)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := bl.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := bl.Append(itob(2), smallVal(2)); !errors.Is(err, ErrBulkLoaderFinished) {
		t.Fatalf("Append after Finish: expected ErrBulkLoaderFinished, got %v", err)
	}
	if _, err := bl.Finish(); !errors.Is(err, ErrBulkLoaderFinished) {
		t.Fatalf("Finish after Finish: expected ErrBulkLoaderFinished, got %v", err)
	}
}

func TestBulkLoadAbort(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	bl, err := bt.BeginBulkLoad(BulkLoadOptions{})
	if err != nil {
		t.Fatalf("BeginBulkLoad: %v", err)
	}
	for i := range 100 {
		if err := bl.Append(itob(i), smallVal(i)); err != nil {
			t.Fatalf("Append(%d): %v", i, err)
		}
	}
	bl.Abort()
	if bt.root != 0 {
		t.Fatalf("Abort left root=%d, expected 0", bt.root)
	}
}

func TestBulkLoadOverflowValues(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	bigVal := func(i int) []byte {
		v := make([]byte, 8000)
		for j := range v {
			v[j] = byte(i + j)
		}
		return v
	}

	bl, err := bt.BeginBulkLoad(BulkLoadOptions{})
	if err != nil {
		t.Fatalf("BeginBulkLoad: %v", err)
	}
	const n = 50
	for i := range n {
		if err := bl.Append(itob(i), bigVal(i)); err != nil {
			t.Fatalf("Append(%d): %v", i, err)
		}
	}
	if _, err := bl.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	for i := range n {
		got, err := bt.Search(itob(i))
		if err != nil {
			t.Fatalf("Search(%d): %v", i, err)
		}
		if !bytes.Equal(got, bigVal(i)) {
			t.Fatalf("Search(%d) returned wrong value", i)
		}
	}
}

func TestBulkLoadBucketRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/bucket-bulk.db"
	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	txn := db.BeginWrite()
	b, err := txn.CreateBucket([]byte("data"))
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	bl, err := b.BeginBulkLoad(BulkLoadOptions{})
	if err != nil {
		t.Fatalf("BeginBulkLoad: %v", err)
	}
	const n = 1000
	for i := range n {
		if err := bl.Append(itob(i), smallVal(i)); err != nil {
			t.Fatalf("Append(%d): %v", i, err)
		}
	}
	if _, err := bl.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	assertValidBtree(t, b.bt)
	if err := txn.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	rtxn := db.BeginRead()
	defer rtxn.Abort()
	rb, err := rtxn.Bucket([]byte("data"))
	if err != nil {
		t.Fatalf("Bucket: %v", err)
	}
	for i := range n {
		got, err := rb.Get(itob(i))
		if err != nil {
			t.Fatalf("Get(%d): %v", i, err)
		}
		if !bytes.Equal(got, smallVal(i)) {
			t.Errorf("Get(%d) returned wrong value", i)
		}
	}
}
