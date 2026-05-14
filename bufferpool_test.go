package frostfire

import (
	"bytes"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func newTestBufferPool(t *testing.T, size int) (*BufferPool, *StorageManager) {
	t.Helper()

	sm, _, err := NewStorageManager(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewStorageManager: %v", err)
	}
	t.Cleanup(func() {
		_ = sm.Close()
	})

	return NewBufferPool(sm, size), sm
}

func writePagePattern(t *testing.T, p *Page, label string) {
	t.Helper()
	clear(p.Data())
	copy(p.Data(), []byte(label))
	p.Data()[PageSize-1] = byte(len(label))
}

func assertPagePattern(t *testing.T, p *Page, label string) {
	t.Helper()
	got := p.Data()[:len(label)]
	if !bytes.Equal(got, []byte(label)) {
		t.Fatalf("page %d data prefix = %q, want %q", p.ID(), got, label)
	}
	if got := p.Data()[PageSize-1]; got != byte(len(label)) {
		t.Fatalf("page %d trailer = %d, want %d", p.ID(), got, len(label))
	}
}

func residentPage(t *testing.T, bp *BufferPool, pageId PageId) *Page {
	t.Helper()

	pageTable := bp.getPageTable(pageId)
	pageTable.mu.Lock()
	p, ok := pageTable.m[pageId]
	pageTable.mu.Unlock()
	if !ok {
		t.Fatalf("page %d is not resident", pageId)
	}
	return p
}

func pageDirty(t *testing.T, p *Page) bool {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dirty
}

func TestBufferPoolReadWriteRoundTrip(t *testing.T) {
	bp, _ := newTestBufferPool(t, 4)

	p, err := bp.AllocatePage(7)
	if err != nil {
		t.Fatalf("AllocatePage: %v", err)
	}
	if got := p.ID(); got != 7 {
		t.Fatalf("allocated page ID = %d, want 7", got)
	}
	writePagePattern(t, p, "hello-bufferpool")
	p.Unpin()

	p, err = bp.Get(7)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer p.Unpin()
	assertPagePattern(t, p, "hello-bufferpool")
}

func TestGetForWriteRequiresExclusiveUnpinnedPage(t *testing.T) {
	bp, _ := newTestBufferPool(t, 2)

	p, err := bp.AllocatePage(1)
	if err != nil {
		t.Fatalf("AllocatePage: %v", err)
	}

	if _, err := bp.GetForWrite(1); !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("GetForWrite pinned page error = %v, want %v", err, ErrInvalidWrite)
	}

	p.Unpin()
	p, err = bp.GetForWrite(1)
	if err != nil {
		t.Fatalf("GetForWrite after unpin: %v", err)
	}
	p.Unpin()
}

func TestPinnedPoolReportsExhaustion(t *testing.T) {
	bp, _ := newTestBufferPool(t, 1)

	p, err := bp.AllocatePage(0)
	if err != nil {
		t.Fatalf("AllocatePage(0): %v", err)
	}
	defer p.Unpin()

	if _, err := bp.AllocatePage(1); !errors.Is(err, ErrBufferExhausted) {
		t.Fatalf("AllocatePage with only frame pinned error = %v, want %v", err, ErrBufferExhausted)
	}
}

func TestEvictionPersistsDirtyVictimBeforeReuse(t *testing.T) {
	bp, _ := newTestBufferPool(t, 1)

	p, err := bp.AllocatePage(0)
	if err != nil {
		t.Fatalf("AllocatePage(0): %v", err)
	}
	writePagePattern(t, p, "dirty-victim")
	p.Unpin()

	p, err = bp.AllocatePage(1)
	if err != nil {
		t.Fatalf("AllocatePage(1): %v", err)
	}
	writePagePattern(t, p, "replacement")
	p.Unpin()

	p, err = bp.Get(0)
	if err != nil {
		t.Fatalf("Get(0): %v", err)
	}
	defer p.Unpin()
	assertPagePattern(t, p, "dirty-victim")
}

func TestMarkCleanPreventsAbortPagesFromFlushing(t *testing.T) {
	bp, _ := newTestBufferPool(t, 1)

	p, err := bp.AllocatePage(0)
	if err != nil {
		t.Fatalf("AllocatePage(0): %v", err)
	}
	writePagePattern(t, p, "aborted")
	p.Unpin()

	bp.MarkClean(0)
	if err := bp.FlushPage(0); err != nil {
		t.Fatalf("FlushPage after MarkClean: %v", err)
	}

	p, err = bp.AllocatePage(1)
	if err != nil {
		t.Fatalf("AllocatePage(1): %v", err)
	}
	p.Unpin()

	p, err = bp.Get(0)
	if err != nil {
		t.Fatalf("Get(0): %v", err)
	}
	defer p.Unpin()
	if bytes.Contains(p.Data(), []byte("aborted")) {
		t.Fatalf("aborted page contents were written to disk")
	}
}

func TestFlushPagesOnlyCleansRequestedDirtyPages(t *testing.T) {
	bp, _ := newTestBufferPool(t, 4)

	for id := range PageId(3) {
		p, err := bp.AllocatePage(id)
		if err != nil {
			t.Fatalf("AllocatePage(%d): %v", id, err)
		}
		writePagePattern(t, p, "dirty")
		p.Unpin()
	}

	if err := bp.FlushPages([]PageId{1}); err != nil {
		t.Fatalf("FlushPages: %v", err)
	}

	if !pageDirty(t, residentPage(t, bp, 0)) {
		t.Fatalf("page 0 should remain dirty")
	}
	if pageDirty(t, residentPage(t, bp, 1)) {
		t.Fatalf("page 1 should have been marked clean")
	}
	if !pageDirty(t, residentPage(t, bp, 2)) {
		t.Fatalf("page 2 should remain dirty")
	}
}

func TestGetPinsMappedPageEvenWhileEvicting(t *testing.T) {
	bp, _ := newTestBufferPool(t, 1)

	p, err := bp.AllocatePage(0)
	if err != nil {
		t.Fatalf("AllocatePage: %v", err)
	}
	writePagePattern(t, p, "resident-during-eviction")
	p.Unpin()

	resident := residentPage(t, bp, 0)
	resident.mu.Lock()
	resident.evicting = true
	resident.mu.Unlock()

	p, err = bp.Get(0)
	if err != nil {
		t.Fatalf("Get evicting mapped page: %v", err)
	}
	assertPagePattern(t, p, "resident-during-eviction")

	p.mu.Lock()
	if p.pinCount != 1 {
		t.Fatalf("pinCount while evicting = %d, want 1", p.pinCount)
	}
	p.evicting = false
	p.mu.Unlock()
	p.Unpin()
}

func TestConcurrentColdGetReturnsOneResidentPageWithIndependentPins(t *testing.T) {
	bp, sm := newTestBufferPool(t, 8)

	if err := sm.Allocate(42); err != nil {
		t.Fatalf("sm.Allocate: %v", err)
	}
	buf := NewBuffer()
	copy(buf, []byte("seeded-on-disk"))
	buf[PageSize-1] = 0x42
	if err := sm.WritePage(42, buf); err != nil {
		t.Fatalf("sm.WritePage: %v", err)
	}

	const readers = 32
	pages := make(chan *Page, readers)
	errs := make(chan error, readers)
	release := make(chan struct{})

	var wg sync.WaitGroup
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := bp.Get(42)
			if err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(p.Data()[:len("seeded-on-disk")], []byte("seeded-on-disk")) || p.Data()[PageSize-1] != 0x42 {
				errs <- errors.New("bad page contents")
				p.Unpin()
				return
			}
			pages <- p
			<-release
			p.Unpin()
		}()
	}

	got := make([]*Page, 0, readers)
	for len(got) < readers {
		select {
		case err := <-errs:
			close(release)
			t.Fatalf("concurrent Get: %v", err)
		case p := <-pages:
			got = append(got, p)
		}
	}

	first := got[0]
	for i, p := range got[1:] {
		if p != first {
			close(release)
			t.Fatalf("reader %d got page pointer %p, want %p", i+1, p, first)
		}
	}

	first.mu.Lock()
	if first.pinCount != readers {
		got := first.pinCount
		first.mu.Unlock()
		close(release)
		t.Fatalf("pinCount after concurrent Gets = %d, want %d", got, readers)
	}
	first.mu.Unlock()

	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Get: %v", err)
	}

	first.mu.Lock()
	if first.pinCount != 0 {
		t.Fatalf("pinCount after releases = %d, want 0", first.pinCount)
	}
	first.mu.Unlock()
}

func TestFlushAllPersistsThroughCloseAndReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reopen.db")

	sm, _, err := NewStorageManager(path)
	if err != nil {
		t.Fatalf("NewStorageManager: %v", err)
	}
	bp := NewBufferPool(sm, 2)

	p, err := bp.AllocatePage(9)
	if err != nil {
		t.Fatalf("AllocatePage: %v", err)
	}
	writePagePattern(t, p, "survives-reopen")
	p.Unpin()

	if err := bp.Close(); err != nil {
		t.Fatalf("BufferPool.Close: %v", err)
	}

	sm, _, err = NewStorageManager(path)
	if err != nil {
		t.Fatalf("reopen NewStorageManager: %v", err)
	}
	defer sm.Close()
	bp = NewBufferPool(sm, 2)

	p, err = bp.Get(9)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	defer p.Unpin()
	assertPagePattern(t, p, "survives-reopen")
}
