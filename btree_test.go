package frostfire

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"testing"
)

func openTestBTreeDB(t *testing.T) (*DB, *Txn, *BTree) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "btree.db")
	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	txn := db.BeginWrite()
	bt := NewBTree(txn, 0)
	t.Cleanup(func() {
		assertValidBtree(t, bt)
		txn.Abort()
		assertAllUnpinned(t, db)
		_ = db.Close()
	})
	return db, txn, bt
}

// itob encodes i as a 4-byte big-endian key so lex order == numeric order.
func itob(i int) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(i))
	return b
}

// smallVal produces a 400-byte value: ~9 cells fit per leaf page before split.
func smallVal(i int) []byte {
	v := make([]byte, 400)
	binary.BigEndian.PutUint32(v, uint32(i))
	return v
}

func assertValidBtree(t *testing.T, bt *BTree) {
	t.Helper()
	root := bt.root
	if root == 0 {
		return
	}

	reachable := make(map[PageId]bool)
	leafDepth := -1

	var traverse func(id PageId, depth int, lo, hi []byte)
	traverse = func(id PageId, depth int, lo, hi []byte) {
		t.Helper()
		if reachable[id] {
			t.Errorf("cycle or shared page at %d", id)
			return
		}
		reachable[id] = true

		node, err := bt.get(id)
		if err != nil {
			t.Fatalf("get(%d): %v", id, err)
		}
		defer node.Unpin()
		nCells := node.nCells()

		for i := uint16(1); i < nCells; i++ {
			if bytes.Compare(node.key(i-1), node.key(i)) >= 0 {
				t.Errorf("page %d: keys not sorted at index %d", id, i)
			}
		}

		if node.ntype() == nodeLeaf {
			if leafDepth == -1 {
				leafDepth = depth
			} else if depth != leafDepth {
				t.Errorf("page %d: leaf at depth %d, expected %d", id, depth, leafDepth)
			}

			if nCells > 0 {
				if lo != nil && bytes.Compare(node.key(0), lo) < 0 {
					t.Errorf("page %d: first key %x below lower bound %x", id, node.key(0), lo)
				}
				if hi != nil && bytes.Compare(node.key(nCells-1), hi) >= 0 {
					t.Errorf("page %d: last key %x at or above upper bound %x", id, node.key(nCells-1), hi)
				}
			}

			for i := range nCells {
				ovID := node.leafCellOverflowId(i)
				for ovID != 0 {
					if reachable[ovID] {
						t.Errorf("overflow page %d referenced more than once", ovID)
						break
					}
					reachable[ovID] = true
					ovNode, err := bt.get(ovID)
					if err != nil {
						t.Fatalf("get overflow page %d: %v", ovID, err)
					}
					if ovNode.ntype() != nodeOverflow {
						t.Errorf("page %d: expected overflow type, got %d", ovID, ovNode.ntype())
					}
					nextOv := PageId(binary.LittleEndian.Uint64(ovNode.data()[1:9]))
					ovNode.Unpin()
					ovID = nextOv
				}
			}
			return
		}

		if nCells == 0 {
			t.Errorf("page %d: internal node with 0 cells", id)
			return
		}
		if lo != nil && bytes.Compare(node.key(0), lo) < 0 {
			t.Errorf("page %d: first key %x below lower bound %x", id, node.key(0), lo)
		}
		if hi != nil && bytes.Compare(node.key(nCells-1), hi) >= 0 {
			t.Errorf("page %d: last key %x at or above upper bound %x", id, node.key(nCells-1), hi)
		}

		for i := uint16(0); i <= nCells; i++ {
			var childLo, childHi []byte
			if i > 0 {
				childLo = node.key(i - 1)
			} else {
				childLo = lo
			}
			if i < nCells {
				childHi = node.key(i)
			} else {
				childHi = hi
			}
			traverse(node.child(i), depth+1, childLo, childHi)
		}
	}

	traverse(root, 0, nil, nil)

	txn := bt.txn
	freed := make(map[PageId]bool)
	for _, id := range txn.freed {
		freed[id] = true
	}
	reusable := make(map[PageId]bool)
	for _, id := range txn.reusable {
		reusable[id] = true
	}
	for _, id := range txn.allocated {
		if freed[id] || reusable[id] {
			continue
		}
		if !reachable[id] {
			t.Errorf("page %d allocated but unreachable from root (leaked)", id)
		}
	}
}

func assertAllUnpinned(t *testing.T, db *DB) {
	t.Helper()
	for i := range db.bufferPool.frames {
		f := db.bufferPool.frames[i]
		if pins := f.pinCount; pins != 0 {
			t.Errorf("frame for page %d has pins=%d, expected 0", f.pageId, pins)
		}
	}
}

func TestInsertSingle(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	if _, err := bt.Update(itob(1), smallVal(1), ModeUpsert); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := bt.Search(itob(1))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !bytes.Equal(got, smallVal(1)) {
		t.Errorf("Search returned wrong value")
	}
}

func TestInsertSequential(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	const n = 100
	for i := range n {
		if _, err := bt.Update(itob(i), smallVal(i), ModeUpsert); err != nil {
			t.Fatalf("Update(%d): %v", i, err)
		}
	}
	for i := range n {
		got, err := bt.Search(itob(i))
		if err != nil {
			t.Fatalf("Search(%d): %v", i, err)
		}
		if !bytes.Equal(got, smallVal(i)) {
			t.Errorf("Search(%d) wrong value", i)
		}
	}
}

func TestInsertReverse(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	const n = 100
	for i := n - 1; i >= 0; i-- {
		if _, err := bt.Update(itob(i), smallVal(i), ModeUpsert); err != nil {
			t.Fatalf("Update(%d): %v", i, err)
		}
	}
	for i := range n {
		got, err := bt.Search(itob(i))
		if err != nil {
			t.Fatalf("Search(%d): %v", i, err)
		}
		if !bytes.Equal(got, smallVal(i)) {
			t.Errorf("Search(%d) wrong value", i)
		}
	}
}

func TestInsertModeInsertDuplicateErrors(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	if _, err := bt.Update(itob(1), smallVal(1), ModeInsert); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	if _, err := bt.Update(itob(1), smallVal(2), ModeInsert); err != ErrKeyExists {
		t.Errorf("expected ErrKeyExists, got %v", err)
	}
}

func TestInsertModeUpdateMissingErrors(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	if _, err := bt.Update(itob(1), smallVal(1), ModeUpdate); err != ErrKeyNotFound {
		t.Errorf("expected ErrKeyNotFound, got %v", err)
	}
}

func TestInsertReplaceValue(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	if _, err := bt.Update(itob(1), smallVal(1), ModeUpsert); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := bt.Update(itob(1), smallVal(99), ModeUpsert); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, err := bt.Search(itob(1))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !bytes.Equal(got, smallVal(99)) {
		t.Errorf("expected replaced value")
	}
}

func TestInsertSearchMissing(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	for i := range 10 {
		if _, err := bt.Update(itob(i), smallVal(i), ModeUpsert); err != nil {
			t.Fatalf("Update(%d): %v", i, err)
		}
	}
	got, err := bt.Search(itob(999))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing key, got %x", got)
	}
}

func TestInsertWithSplits(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	// 500 keys with ~400-byte values forces many leaf splits and at least
	// one internal-node split.
	const n = 500
	for i := range n {
		if _, err := bt.Update(itob(i), smallVal(i), ModeUpsert); err != nil {
			t.Fatalf("Update(%d): %v", i, err)
		}
	}
	for i := range n {
		got, err := bt.Search(itob(i))
		if err != nil {
			t.Fatalf("Search(%d): %v", i, err)
		}
		if !bytes.Equal(got, smallVal(i)) {
			t.Errorf("Search(%d) wrong value", i)
		}
	}
}

func TestDeleteSingle(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	if _, err := bt.Update(itob(1), smallVal(1), ModeUpsert); err != nil {
		t.Fatalf("Update: %v", err)
	}
	_, found, err := bt.Delete(itob(1))
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !found {
		t.Errorf("Delete: expected found=true")
	}
	got, err := bt.Search(itob(1))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil after delete, got %x", got)
	}
}

func TestDeleteMissing(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	for i := range 10 {
		if _, err := bt.Update(itob(i), smallVal(i), ModeUpsert); err != nil {
			t.Fatalf("Update(%d): %v", i, err)
		}
	}
	_, found, err := bt.Delete(itob(999))
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if found {
		t.Errorf("Delete: expected found=false for missing key")
	}
}

func TestDeleteAllSequential(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	const n = 200
	for i := range n {
		if _, err := bt.Update(itob(i), smallVal(i), ModeUpsert); err != nil {
			t.Fatalf("Update(%d): %v", i, err)
		}
	}
	for i := range n {
		_, found, err := bt.Delete(itob(i))
		if err != nil {
			t.Fatalf("Delete(%d): %v", i, err)
		}
		if !found {
			t.Errorf("Delete(%d): expected found=true", i)
		}
	}
	if bt.root != 0 {
		t.Errorf("expected empty tree, root=%d", bt.root)
	}
}

func TestDeleteAllReverse(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	const n = 200
	for i := range n {
		if _, err := bt.Update(itob(i), smallVal(i), ModeUpsert); err != nil {
			t.Fatalf("Update(%d): %v", i, err)
		}
	}
	for i := n - 1; i >= 0; i-- {
		_, found, err := bt.Delete(itob(i))
		if err != nil {
			t.Fatalf("Delete(%d): %v", i, err)
		}
		if !found {
			t.Errorf("Delete(%d): expected found=true", i)
		}
	}
	if bt.root != 0 {
		t.Errorf("expected empty tree, root=%d", bt.root)
	}
}

func TestDeletePartialKeepsOthers(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	const n = 200
	for i := range n {
		if _, err := bt.Update(itob(i), smallVal(i), ModeUpsert); err != nil {
			t.Fatalf("Update(%d): %v", i, err)
		}
	}
	// Delete every other key.
	for i := 0; i < n; i += 2 {
		if _, found, err := bt.Delete(itob(i)); err != nil {
			t.Fatalf("Delete(%d): %v", i, err)
		} else if !found {
			t.Errorf("Delete(%d): expected found=true", i)
		}
	}
	for i := range n {
		got, err := bt.Search(itob(i))
		if err != nil {
			t.Fatalf("Search(%d): %v", i, err)
		}
		if i%2 == 0 {
			if got != nil {
				t.Errorf("Search(%d): expected nil after delete, got non-nil", i)
			}
		} else {
			if got == nil {
				t.Errorf("Search(%d): expected value, got nil", i)
			}
		}
	}
}

func TestDeleteOverflowValueFreesPages(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	big := make([]byte, 16384)
	for i := range big {
		big[i] = byte(i % 251)
	}
	if _, err := bt.Update(itob(1), big, ModeUpsert); err != nil {
		t.Fatalf("Update: %v", err)
	}
	_, found, err := bt.Delete(itob(1))
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !found {
		t.Errorf("Delete: expected found=true")
	}
	got, err := bt.Search(itob(1))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil after delete, got len=%d", len(got))
	}
}

func TestInsertOverflowValue(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)

	big := make([]byte, 16384)
	for i := range big {
		big[i] = byte(i % 251)
	}
	if _, err := bt.Update(itob(1), big, ModeUpsert); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := bt.Search(itob(1))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !bytes.Equal(got, big) {
		t.Errorf("overflow value mismatch (len got=%d want=%d)", len(got), len(big))
	}
}
