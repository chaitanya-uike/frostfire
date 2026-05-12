package frostfire

import (
	"bytes"
	"path/filepath"
	"testing"
)

func newTestPool(t *testing.T, size int) (*BufferPool, *StorageManager) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	sm, _, err := NewStorageManager(path)
	if err != nil {
		t.Fatalf("NewStorageManager: %v", err)
	}
	t.Cleanup(func() { sm.Close() })
	return NewBufferPool(sm, size), sm
}

func fillPool(t *testing.T, bp *BufferPool, startId PageId, n int) {
	t.Helper()
	for i := range n {
		p, err := bp.AllocatePage(startId + PageId(i))
		if err != nil {
			t.Fatalf("AllocatePage(%d): %v", startId+PageId(i), err)
		}
		p.Unpin()
	}
}

func TestAllocateThenGetReturnsSameData(t *testing.T) {
	bp, _ := newTestPool(t, 4)

	pg, err := bp.AllocatePage(0)
	if err != nil {
		t.Fatalf("AllocatePage: %v", err)
	}
	want := []byte("hello frostfire")
	copy(pg.data, want)
	pg.Unpin()

	got, err := bp.Get(0)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer got.Unpin()
	if !bytes.Equal(got.data[:len(want)], want) {
		t.Fatalf("got %q want %q", got.data[:len(want)], want)
	}
}

func TestGetReturnsSameFrameForSameId(t *testing.T) {
	bp, _ := newTestPool(t, 4)
	p, err := bp.AllocatePage(7)
	if err != nil {
		t.Fatalf("AllocatePage: %v", err)
	}
	p.Unpin()

	a, err := bp.Get(7)
	if err != nil {
		t.Fatalf("Get a: %v", err)
	}
	defer a.Unpin()
	b, err := bp.Get(7)
	if err != nil {
		t.Fatalf("Get b: %v", err)
	}
	defer b.Unpin()
	if a != b {
		t.Fatalf("Get returned different *Page for same id")
	}

	want := []byte("shared")
	copy(a.data, want)
	if !bytes.Equal(b.data[:len(want)], want) {
		t.Fatalf("mutation not visible across handles")
	}
}

func TestBufferExhaustedWhenAllPinned(t *testing.T) {
	bp, _ := newTestPool(t, 2)
	p0, err := bp.AllocatePage(0)
	if err != nil {
		t.Fatalf("AllocatePage(0): %v", err)
	}
	p1, err := bp.AllocatePage(1)
	if err != nil {
		t.Fatalf("AllocatePage(1): %v", err)
	}

	if _, err := bp.AllocatePage(2); err != ErrBufferExhausted {
		t.Fatalf("got %v want ErrBufferExhausted", err)
	}

	p0.Unpin()
	p1.Unpin()
	if _, err := bp.AllocatePage(2); err != nil {
		t.Fatalf("AllocatePage(2) after unpin: %v", err)
	}
}

func TestUnpinPanicsWhenUnpinned(t *testing.T) {
	bp, _ := newTestPool(t, 2)
	pg, err := bp.AllocatePage(0)
	if err != nil {
		t.Fatalf("AllocatePage: %v", err)
	}
	pg.Unpin()

	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on excess Unpin")
		}
	}()
	pg.Unpin()
}

func TestFlushPagePersistsToDisk(t *testing.T) {
	bp, sm := newTestPool(t, 2)
	pg, err := bp.AllocatePage(3)
	if err != nil {
		t.Fatalf("AllocatePage: %v", err)
	}
	want := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	copy(pg.data, want)
	pg.Unpin()

	if err := bp.FlushPage(3); err != nil {
		t.Fatalf("FlushPage: %v", err)
	}

	buf := NewBuffer()
	if err := sm.ReadPage(3, buf); err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if !bytes.Equal(buf[:len(want)], want) {
		t.Fatalf("got %v want %v", buf[:len(want)], want)
	}
}

func TestFlushAllPersistsAllDirtyPages(t *testing.T) {
	bp, sm := newTestPool(t, 8)

	wants := map[PageId][]byte{
		0: []byte("page-zero"),
		1: []byte("page-one"),
		2: []byte("page-two"),
		3: []byte("page-three"),
	}
	for id, want := range wants {
		pg, err := bp.AllocatePage(id)
		if err != nil {
			t.Fatalf("AllocatePage(%d): %v", id, err)
		}
		copy(pg.data, want)
		pg.Unpin()
	}

	if err := bp.FlushAll(); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}

	for id, want := range wants {
		buf := NewBuffer()
		if err := sm.ReadPage(id, buf); err != nil {
			t.Fatalf("ReadPage(%d): %v", id, err)
		}
		if !bytes.Equal(buf[:len(want)], want) {
			t.Fatalf("page %d: got %q want %q", id, buf[:len(want)], want)
		}
	}
}

func TestPinnedPageSurvivesPressure(t *testing.T) {
	bp, _ := newTestPool(t, 4)

	pinned, err := bp.AllocatePage(0)
	if err != nil {
		t.Fatalf("AllocatePage(0): %v", err)
	}
	want := []byte("survive me")
	copy(pinned.data, want)

	fillPool(t, bp, 100, 64)

	if !bytes.Equal(pinned.data[:len(want)], want) {
		t.Fatalf("pinned page corrupted: got %q want %q", pinned.data[:len(want)], want)
	}

	again, err := bp.Get(0)
	if err != nil {
		t.Fatalf("Get(0) after churn: %v", err)
	}
	defer again.Unpin()
	if again != pinned {
		t.Fatalf("pinned page was evicted: Get returned a different *Page")
	}

	pinned.Unpin()
}

func TestEvictedPageReloadsFromDisk(t *testing.T) {
	bp, _ := newTestPool(t, 4)

	pg, err := bp.AllocatePage(0)
	if err != nil {
		t.Fatalf("AllocatePage: %v", err)
	}
	want := []byte("evict-and-reload")
	copy(pg.data, want)
	pg.Unpin()
	if err := bp.FlushPage(0); err != nil {
		t.Fatalf("FlushPage: %v", err)
	}

	fillPool(t, bp, 100, 64)

	got, err := bp.Get(0)
	if err != nil {
		t.Fatalf("Get(0) after churn: %v", err)
	}
	defer got.Unpin()
	if !bytes.Equal(got.data[:len(want)], want) {
		t.Fatalf("got %q want %q", got.data[:len(want)], want)
	}
}

func TestDirtyEvictionFlushesBeforeReuse(t *testing.T) {
	bp, sm := newTestPool(t, 4)

	pg, err := bp.AllocatePage(0)
	if err != nil {
		t.Fatalf("AllocatePage: %v", err)
	}
	want := []byte("dirty-evicted")
	copy(pg.data, want)
	pg.Unpin()

	fillPool(t, bp, 100, 64)

	buf := NewBuffer()
	if err := sm.ReadPage(0, buf); err != nil {
		t.Fatalf("ReadPage(0): %v", err)
	}
	if !bytes.Equal(buf[:len(want)], want) {
		t.Fatalf("dirty page was not flushed on eviction: got %q want %q", buf[:len(want)], want)
	}
}

func TestGetForWriteMakesPageDirty(t *testing.T) {
	bp, sm := newTestPool(t, 4)

	pg, err := bp.AllocatePage(0)
	if err != nil {
		t.Fatalf("AllocatePage: %v", err)
	}
	original := []byte("original-contents")
	copy(pg.data, original)
	pg.Unpin()
	if err := bp.FlushPage(0); err != nil {
		t.Fatalf("FlushPage: %v", err)
	}

	w, err := bp.GetForWrite(0)
	if err != nil {
		t.Fatalf("GetForWrite: %v", err)
	}
	w.Unpin()

	fillPool(t, bp, 100, 64)

	buf := NewBuffer()
	if err := sm.ReadPage(0, buf); err != nil {
		t.Fatalf("ReadPage(0): %v", err)
	}
	zero := make([]byte, PageSize)
	if !bytes.Equal(buf, zero) {
		t.Fatalf("GetForWrite did not mark page dirty: disk still has prior contents")
	}
}

func TestFlushAllSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reopen.db")

	sm, _, err := NewStorageManager(path)
	if err != nil {
		t.Fatalf("NewStorageManager: %v", err)
	}
	bp := NewBufferPool(sm, 4)
	want := []byte("durable")
	pg, err := bp.AllocatePage(2)
	if err != nil {
		t.Fatalf("AllocatePage: %v", err)
	}
	copy(pg.data, want)
	pg.Unpin()
	if err := bp.FlushAll(); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}
	if err := sm.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := sm.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sm2, _, err := NewStorageManager(path)
	if err != nil {
		t.Fatalf("reopen NewStorageManager: %v", err)
	}
	defer sm2.Close()
	bp2 := NewBufferPool(sm2, 4)
	got, err := bp2.Get(2)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	defer got.Unpin()
	if !bytes.Equal(got.data[:len(want)], want) {
		t.Fatalf("after reopen: got %q want %q", got.data[:len(want)], want)
	}
}
