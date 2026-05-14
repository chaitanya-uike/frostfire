package frostfire

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/errgroup"
)

const (
	tableShards         = 128
	maxUsageCount       = 5
	maxFlushConcurrency = 16
	freePageId          = PageId(^uint64(0))
)

var (
	ErrBufferExhausted = errors.New("BufferPool: no unpinned buffers available")
	ErrInvalidWrite    = errors.New("BufferPool: cannot write over pinned or evicting page")
)

type Page struct {
	mu sync.Mutex

	pageId PageId
	data   []byte

	pinCount   int32
	usageCount int32
	dirty      bool
	evicting   bool
}

type PageTable struct {
	mu sync.Mutex
	m  map[PageId]*Page
}

type loadingEntry struct {
	done chan struct{}
}

type BufferPool struct {
	frames    []Page
	shards    [tableShards]PageTable
	loading   sync.Map // PageId -> *loadingEntry
	clockHand atomic.Uint32
	sm        *StorageManager
}

func (p *Page) ID() PageId {
	return p.pageId
}

func (p *Page) Data() []byte {
	return p.data
}

func (p *Page) Unpin() {
	p.mu.Lock()
	if p.pinCount <= 0 {
		p.mu.Unlock()
		panic("bufferpool: Release on page with pinCount <= 0")
	}
	p.pinCount--
	p.mu.Unlock()
}

func (p *Page) Release() {
	p.Unpin()
}

func NewBufferPool(sm *StorageManager, size int) *BufferPool {
	bp := &BufferPool{
		frames: make([]Page, size),
		sm:     sm,
	}

	for i := range bp.frames {
		f := &bp.frames[i]
		f.pageId = freePageId
		f.data = NewBuffer()
	}

	for i := range bp.shards {
		bp.shards[i].m = make(map[PageId]*Page)
	}

	return bp
}

func (bp *BufferPool) getPageTable(pageId PageId) *PageTable {
	return &bp.shards[pageId%tableShards]
}

func (bp *BufferPool) Get(pageId PageId) (*Page, error) {
	for {
		pageTable := bp.getPageTable(pageId)
		pageTable.mu.Lock()
		if frame, ok := pageTable.m[pageId]; ok {
			frame.mu.Lock()
			pageTable.mu.Unlock()

			frame.pinCount++
			frame.usageCount = maxUsageCount
			frame.mu.Unlock()
			return frame, nil
		}
		pageTable.mu.Unlock()

		entry, loaded := bp.loading.LoadOrStore(pageId, &loadingEntry{done: make(chan struct{})})
		if loaded {
			<-entry.(*loadingEntry).done
			continue
		}

		loader := entry.(*loadingEntry)
		// We may have missed the page before a previous loader installed it.
		// Recheck before doing disk I/O to avoid duplicate loads.
		pageTable = bp.getPageTable(pageId)
		pageTable.mu.Lock()
		if _, ok := pageTable.m[pageId]; ok {
			pageTable.mu.Unlock()
			close(loader.done)
			bp.loading.Delete(pageId)
			continue
		}
		pageTable.mu.Unlock()

		page, err := bp.loadAndInstall(pageId)
		close(loader.done)
		bp.loading.Delete(pageId)
		if err != nil {
			return nil, err
		}
		return page, nil
	}
}

func (bp *BufferPool) loadAndInstall(pageId PageId) (*Page, error) {
	frame, err := bp.acquireFrame()
	if err != nil {
		return nil, err
	}

	if err := bp.sm.ReadPage(pageId, frame.data); err != nil {
		bp.releaseToFree(frame)
		return nil, err
	}

	pageTable := bp.getPageTable(pageId)
	pageTable.mu.Lock()
	frame.mu.Lock()
	frame.pageId = pageId
	frame.pinCount = 1
	frame.usageCount = maxUsageCount
	frame.evicting = false
	pageTable.m[pageId] = frame
	frame.mu.Unlock()
	pageTable.mu.Unlock()

	return frame, nil
}

func (bp *BufferPool) releaseToFree(frame *Page) {
	frame.mu.Lock()
	frame.pageId = freePageId
	frame.pinCount = 0
	frame.usageCount = 0
	frame.dirty = false
	frame.evicting = false
	frame.mu.Unlock()
}

func (bp *BufferPool) GetForWrite(pageId PageId) (*Page, error) {
	pageTable := bp.getPageTable(pageId)

	pageTable.mu.Lock()
	if frame, exists := pageTable.m[pageId]; exists {
		frame.mu.Lock()
		pageTable.mu.Unlock()

		if frame.evicting || frame.pinCount > 0 {
			frame.mu.Unlock()
			return nil, ErrInvalidWrite
		}
		frame.pinCount = 1
		frame.usageCount = maxUsageCount
		frame.dirty = true
		frame.mu.Unlock()
		return frame, nil
	}
	pageTable.mu.Unlock()

	frame, err := bp.acquireFrame()
	if err != nil {
		return nil, err
	}

	pageTable.mu.Lock()
	frame.mu.Lock()
	frame.pageId = pageId
	frame.pinCount = 1
	frame.usageCount = maxUsageCount
	frame.dirty = true
	frame.evicting = false
	pageTable.m[pageId] = frame
	frame.mu.Unlock()
	pageTable.mu.Unlock()

	return frame, nil
}

func (bp *BufferPool) AllocatePage(pageId PageId) (*Page, error) {
	if err := bp.sm.Allocate(pageId); err != nil {
		return nil, err
	}
	return bp.GetForWrite(pageId)
}

func (bp *BufferPool) FlushPage(pageId PageId) error {
	pageTable := bp.getPageTable(pageId)
	pageTable.mu.Lock()
	frame, ok := pageTable.m[pageId]
	pageTable.mu.Unlock()
	if !ok {
		return nil
	}

	frame.mu.Lock()
	if frame.evicting || !frame.dirty {
		frame.mu.Unlock()
		return nil
	}
	frame.mu.Unlock()

	if err := bp.sm.WritePage(pageId, frame.data); err != nil {
		return err
	}

	frame.mu.Lock()
	if !frame.evicting {
		frame.dirty = false
	}
	frame.mu.Unlock()
	return nil
}

func (bp *BufferPool) FlushPages(pageIds []PageId) error {
	dirty := make([]*Page, 0, len(pageIds))
	for _, pageId := range pageIds {
		pageTable := bp.getPageTable(pageId)
		pageTable.mu.Lock()
		frame, ok := pageTable.m[pageId]
		pageTable.mu.Unlock()
		if !ok {
			continue
		}

		frame.mu.Lock()
		if frame.evicting || !frame.dirty {
			frame.mu.Unlock()
			continue
		}
		frame.mu.Unlock()
		dirty = append(dirty, frame)
	}
	return bp.flushDirty(dirty)
}

func (bp *BufferPool) FlushAll() error {
	dirty := make([]*Page, 0, len(bp.frames))
	for i := range bp.frames {
		frame := &bp.frames[i]

		frame.mu.Lock()
		if frame.evicting || !frame.dirty {
			frame.mu.Unlock()
			continue
		}
		frame.mu.Unlock()
		dirty = append(dirty, frame)
	}
	return bp.flushDirty(dirty)
}

func (bp *BufferPool) MarkClean(pageId PageId) {
	pageTable := bp.getPageTable(pageId)
	pageTable.mu.Lock()
	frame, ok := pageTable.m[pageId]
	if !ok {
		pageTable.mu.Unlock()
		return
	}

	frame.mu.Lock()
	pageTable.mu.Unlock()
	frame.dirty = false
	frame.mu.Unlock()
}

func (bp *BufferPool) Sync() error {
	return bp.sm.Sync()
}

func (bp *BufferPool) Close() error {
	flushErr := bp.FlushAll()
	syncErr := bp.sm.Sync()
	closeErr := bp.sm.Close()
	return errors.Join(flushErr, syncErr, closeErr)
}

func (bp *BufferPool) flushDirty(dirty []*Page) error {
	if len(dirty) == 0 {
		return nil
	}

	slices.SortFunc(dirty, func(a, b *Page) int {
		return cmp.Compare(a.pageId, b.pageId)
	})

	g, ctx := errgroup.WithContext(context.Background())
	g.SetLimit(maxFlushConcurrency)

	for i := 0; i < len(dirty); {
		j := i + 1
		for j < len(dirty) && dirty[j].pageId == dirty[j-1].pageId+1 {
			j++
		}
		frames := dirty[i:j]
		i = j

		g.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			bufs := make([][]byte, len(frames))
			for k, f := range frames {
				bufs[k] = f.data
			}
			if err := bp.sm.WritePagesContiguous(frames[0].pageId, bufs); err != nil {
				return err
			}
			for _, f := range frames {
				f.mu.Lock()
				if !f.evicting {
					f.dirty = false
				}
				f.mu.Unlock()
			}
			return nil
		})
	}

	return g.Wait()
}

func (bp *BufferPool) acquireFrame() (*Page, error) {
	for {
		victim, err := bp.evictFrame()
		if err != nil {
			return nil, err
		}

		victim.mu.Lock()
		if victim.pageId == freePageId {
			victim.mu.Unlock()
			return victim, nil
		}

		oldPageId := victim.pageId
		dirty := victim.dirty
		victim.mu.Unlock()

		if dirty {
			// Keep the old page mapped while writing it back so concurrent
			// readers get the latest in-memory bytes, not stale disk bytes.
			if err := bp.sm.WritePage(oldPageId, victim.data); err != nil {
				victim.mu.Lock()
				victim.evicting = false
				victim.mu.Unlock()
				return nil, err
			}
		}

		pageTable := bp.getPageTable(oldPageId)
		pageTable.mu.Lock()
		victim.mu.Lock()
		if victim.pinCount > 0 {
			victim.dirty = false
			victim.evicting = false
			victim.mu.Unlock()
			pageTable.mu.Unlock()
			continue
		}
		delete(pageTable.m, oldPageId)
		victim.pageId = freePageId
		victim.dirty = false
		victim.mu.Unlock()
		pageTable.mu.Unlock()

		return victim, nil
	}
}

func (bp *BufferPool) evictFrame() (*Page, error) {
	nFrames := len(bp.frames)
	tries := nFrames

	for {
		frame := &bp.frames[(bp.clockHand.Add(1)-1)%uint32(len(bp.frames))]
		frame.mu.Lock()

		if frame.evicting {
			frame.mu.Unlock()
			continue
		}

		if frame.pageId == freePageId {
			frame.evicting = true
			frame.mu.Unlock()
			return frame, nil
		}

		if frame.pinCount == 0 {
			if frame.usageCount != 0 {
				frame.usageCount--
				tries = nFrames
				frame.mu.Unlock()
				continue
			} else {
				frame.evicting = true
				frame.mu.Unlock()
				return frame, nil
			}
		}

		tries--
		if tries == 0 {
			frame.mu.Unlock()
			return nil, ErrBufferExhausted
		}
		frame.mu.Unlock()
	}
}
