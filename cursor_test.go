package frostfire

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestCursorSingleKey(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)
	if _, err := bt.Update(itob(7), smallVal(7), ModeUpsert); err != nil {
		t.Fatalf("Update: %v", err)
	}

	c := bt.Cursor()
	defer c.Close()

	k, v := c.First()
	if !bytes.Equal(k, itob(7)) {
		t.Errorf("First.k: %x, want %x", k, itob(7))
	}
	if !bytes.Equal(v, smallVal(7)) {
		t.Errorf("First.v wrong")
	}
	if k, _ := c.Next(); k != nil {
		t.Errorf("Next after only key: got %x, want nil", k)
	}
}

func TestCursorForwardWalk(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)
	const n = 500
	for i := range n {
		if _, err := bt.Update(itob(i), smallVal(i), ModeUpsert); err != nil {
			t.Fatalf("Update(%d): %v", i, err)
		}
	}

	c := bt.Cursor()
	defer c.Close()

	count := 0
	for k, v := c.First(); k != nil; k, v = c.Next() {
		want := uint32(count)
		gotKey := binary.BigEndian.Uint32(k)
		if gotKey != want {
			t.Fatalf("at %d: got key %d, want %d", count, gotKey, want)
		}
		if !bytes.Equal(v, smallVal(count)) {
			t.Fatalf("at %d: bad value", count)
		}
		count++
	}
	if count != n {
		t.Errorf("walked %d, want %d", count, n)
	}
}

func TestCursorBackwardWalk(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)
	const n = 500
	for i := range n {
		if _, err := bt.Update(itob(i), smallVal(i), ModeUpsert); err != nil {
			t.Fatalf("Update(%d): %v", i, err)
		}
	}

	c := bt.Cursor()
	defer c.Close()

	count := n - 1
	for k, v := c.Last(); k != nil; k, v = c.Prev() {
		want := uint32(count)
		gotKey := binary.BigEndian.Uint32(k)
		if gotKey != want {
			t.Fatalf("at %d: got key %d, want %d", count, gotKey, want)
		}
		if !bytes.Equal(v, smallVal(count)) {
			t.Fatalf("at %d: bad value", count)
		}
		count--
	}
	if count != -1 {
		t.Errorf("ended at count=%d, want -1", count)
	}
}

func TestCursorSeekExact(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)
	for i := range 100 {
		if _, err := bt.Update(itob(i), smallVal(i), ModeUpsert); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}

	c := bt.Cursor()
	defer c.Close()

	for i := range 100 {
		k, v := c.Seek(itob(i))
		gotKey := binary.BigEndian.Uint32(k)
		if gotKey != uint32(i) {
			t.Errorf("Seek(%d) key: got %d", i, gotKey)
		}
		if !bytes.Equal(v, smallVal(i)) {
			t.Errorf("Seek(%d) value mismatch", i)
		}
	}
}

func TestCursorSeekPastEnd(t *testing.T) {
	_, _, bt := openTestBTreeDB(t)
	for i := range 50 {
		if _, err := bt.Update(itob(i), smallVal(i), ModeUpsert); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}

	c := bt.Cursor()
	defer c.Close()

	if k, _ := c.Seek(itob(999)); k != nil {
		t.Errorf("Seek past end: got %x, want nil", k)
	}
}

func TestCursorOnBucket(t *testing.T) {
	db, _ := openTestDBOnly(t)
	wt := db.BeginWrite()
	defer wt.Abort()
	b, err := wt.CreateBucket([]byte("data"))
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	for i := range 30 {
		if err := b.Put(itob(i), smallVal(i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	c := b.Cursor()
	defer c.Close()
	count := 0
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		count++
	}
	if count != 30 {
		t.Errorf("walked %d, want 30", count)
	}
}
