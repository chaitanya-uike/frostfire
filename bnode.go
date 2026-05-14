package frostfire

import "encoding/binary"

// B-tree pages use slotted page format. A fixed header and a growing array of
// 2-byte cell offsets live at the front of the page. Variable sized cells are
// packed from the end of the page backward. firstCellOffset points at the
// lowest-addressed cell payload, so free space is the gap between the offset
// array and the cell area.
//
// Leaf page:
//
//	+---------+----------+-----------------+------------------+------+------+
//	| byte 0  | bytes 1-2| bytes 3-4       | 2B offsets ...   | free | cells|
//	| type=2  | nCells   | firstCellOffset | one per cell     | gap  | ...  |
//	+---------+----------+-----------------+------------------+------+------+
//
// Leaf cell:
//
//	+----------+----------+----------+---------+---------------------------+
//	| bytes 0-1| bytes 2-3| byte 4   | key     | value or overflow page id |
//	| keyLen   | valueLen | overflow | keyLenB | valueLenB inline, or 8B   |
//	+----------+----------+----------+---------+---------------------------+
//
// Internal page:
//
//	+---------+----------+-----------------+----------------+---------+------+------+
//	| byte 0  | bytes 1-2| bytes 3-4       | bytes 5-12     | offsets | free | cells|
//	| type=1  | nCells   | firstCellOffset | rightmostChild | 2B each | gap  | ...  |
//	+---------+----------+-----------------+----------------+---------+------+------+
//
// Internal cell:
//
//	+-----------+----------+---------+
//	| bytes 0-7 | bytes 8-9| key     |
//	| leftChild | keyLen   | keyLenB |
//	+-----------+----------+---------+

const (
	nodeInternal uint8 = 1
	nodeLeaf     uint8 = 2
	nodeOverflow uint8 = 3
)

const (
	leafHeaderSize     = 5
	internalHeaderSize = 13
	leafCellHeader     = 5
	internalCellHeader = 10
	minCellsPerPage    = 4
	leafCellCost       = leafCellHeader + 2
	internalCellCost   = internalCellHeader + 2
)

const (
	maxKeySize    = (PageSize - internalHeaderSize - internalCellCost*minCellsPerPage) / minCellsPerPage
	maxInlineSize = (PageSize - leafHeaderSize - leafCellCost*minCellsPerPage) / minCellsPerPage
)

type bnode struct {
	page *Page
}

func (n *bnode) data() []byte { return n.page.data }
func (n *bnode) Id() PageId   { return n.page.pageId }
func (n *bnode) Unpin()       { n.page.Unpin() }

func (n *bnode) ntype() uint8    { return n.data()[0] }
func (n *bnode) setType(t uint8) { n.data()[0] = t }

func (n *bnode) nCells() uint16     { return binary.LittleEndian.Uint16(n.data()[1:3]) }
func (n *bnode) setNCells(c uint16) { binary.LittleEndian.PutUint16(n.data()[1:3], c) }

func (n *bnode) firstCellOffset() uint16       { return binary.LittleEndian.Uint16(n.data()[3:5]) }
func (n *bnode) setFirstCellOffset(off uint16) { binary.LittleEndian.PutUint16(n.data()[3:5], off) }

func (n *bnode) headerSize() uint16 {
	if n.ntype() == nodeLeaf {
		return leafHeaderSize
	}
	return internalHeaderSize
}

func (n *bnode) cellOffset(idx uint16) uint16 {
	assert(idx < n.nCells(), "cellOffset: index out of bounds")
	pos := n.headerSize() + idx*2
	return binary.LittleEndian.Uint16(n.data()[pos : pos+2])
}

func (n *bnode) key(idx uint16) []byte {
	pos := n.cellOffset(idx)
	d := n.data()
	if n.ntype() == nodeLeaf {
		keylen := binary.LittleEndian.Uint16(d[pos : pos+2])
		keyStart := pos + leafCellHeader
		return d[keyStart : keyStart+keylen]
	}
	keylen := binary.LittleEndian.Uint16(d[pos+8 : pos+10])
	keyStart := pos + internalCellHeader
	return d[keyStart : keyStart+keylen]
}

func (n *bnode) totalCellsSize() uint16 {
	return PageSize - n.firstCellOffset()
}

func (n *bnode) freeSpace() uint16 {
	offsetArrayEnd := n.headerSize() + n.nCells()*2
	return n.firstCellOffset() - offsetArrayEnd
}

func (n *bnode) underfull() bool {
	if n.nCells() < minCellsPerPage {
		return true
	}
	used := PageSize - n.freeSpace()
	return used < PageSize/4
}

func (n *bnode) cellSize(idx uint16) uint16 {
	pos := n.cellOffset(idx)
	d := n.data()
	if n.ntype() == nodeLeaf {
		keylen := binary.LittleEndian.Uint16(d[pos : pos+2])
		vallen := binary.LittleEndian.Uint16(d[pos+2 : pos+4])
		size := uint16(leafCellHeader) + keylen
		if d[pos+4] == 1 {
			size += 8
		} else {
			size += vallen
		}
		return size
	}
	keylen := binary.LittleEndian.Uint16(d[pos+8 : pos+10])
	return internalCellHeader + keylen
}

func leafCellSize(key, value []byte) uint16 {
	inlineSize := leafCellHeader + len(key) + len(value)
	if inlineSize <= maxInlineSize {
		return uint16(inlineSize)
	}
	return uint16(leafCellHeader + len(key) + 8)
}

func internalCellSize(key []byte) uint16 {
	return uint16(internalCellHeader + len(key))
}

func leafCellSizeAt(data []byte, idx uint16) uint16 {
	offPos := leafHeaderSize + idx*2
	pos := binary.LittleEndian.Uint16(data[offPos : offPos+2])
	keylen := binary.LittleEndian.Uint16(data[pos : pos+2])
	vallen := binary.LittleEndian.Uint16(data[pos+2 : pos+4])
	size := uint16(leafCellHeader) + keylen
	if data[pos+4] == 1 {
		size += 8
	} else {
		size += vallen
	}
	return size
}

func internalCellSizeAt(data []byte, idx uint16) uint16 {
	offPos := internalHeaderSize + idx*2
	pos := binary.LittleEndian.Uint16(data[offPos : offPos+2])
	keylen := binary.LittleEndian.Uint16(data[pos+8 : pos+10])
	return internalCellHeader + keylen
}

func (n *bnode) child(idx uint16) PageId {
	assert(n.ntype() == nodeInternal, "child: not internal")
	assert(idx <= n.nCells(), "child: index out of bounds")
	if idx == n.nCells() {
		return n.rightmostChild()
	}
	pos := n.cellOffset(idx)
	return PageId(binary.LittleEndian.Uint64(n.data()[pos : pos+8]))
}

func (n *bnode) setChild(idx uint16, id PageId) {
	assert(n.ntype() == nodeInternal, "setChild: not internal")
	assert(idx <= n.nCells(), "setChild: index out of bounds")
	if idx == n.nCells() {
		n.setRightmostChild(id)
		return
	}
	pos := n.cellOffset(idx)
	binary.LittleEndian.PutUint64(n.data()[pos:pos+8], uint64(id))
}

func (n *bnode) rightmostChild() PageId {
	return PageId(binary.LittleEndian.Uint64(n.data()[5:13]))
}

func (n *bnode) setRightmostChild(id PageId) {
	binary.LittleEndian.PutUint64(n.data()[5:13], uint64(id))
}

func (n *bnode) appendCell(cell []byte) {
	cellLen := uint16(len(cell))
	assert(n.freeSpace() >= 2+cellLen, "appendCell: not enough free space")

	d := n.data()
	newCellPos := n.firstCellOffset() - cellLen
	copy(d[newCellPos:], cell)

	idx := n.nCells()
	offsetPos := n.headerSize() + idx*2
	binary.LittleEndian.PutUint16(d[offsetPos:offsetPos+2], newCellPos)

	n.setFirstCellOffset(newCellPos)
	n.setNCells(idx + 1)
}

func (dst *bnode) copyCell(src *bnode, srcIdx uint16) {
	pos := src.cellOffset(srcIdx)
	size := src.cellSize(srcIdx)
	dst.appendCell(src.data()[pos : pos+size])
}

func (dst *bnode) copyCellAt(dstIdx uint16, src *bnode, srcIdx uint16) {
	nCells := dst.nCells()
	assert(dstIdx <= nCells, "copyCellAt: dstIdx out of bounds")

	pos := src.cellOffset(srcIdx)
	cellLen := src.cellSize(srcIdx)
	assert(dst.freeSpace() >= 2+cellLen, "copyCellAt: not enough free space")

	d := dst.data()
	newCellPos := dst.firstCellOffset() - cellLen
	copy(d[newCellPos:], src.data()[pos:pos+cellLen])

	offsetPos := dst.headerSize() + dstIdx*2
	if dstIdx < nCells {
		tail := d[offsetPos : dst.headerSize()+nCells*2]
		copy(d[offsetPos+2:], tail)
	}
	binary.LittleEndian.PutUint16(d[offsetPos:offsetPos+2], newCellPos)

	dst.setFirstCellOffset(newCellPos)
	dst.setNCells(nCells + 1)
}

func (dst *bnode) copyRange(src *bnode, srcStart, count uint16) {
	assert(src.ntype() == dst.ntype(), "copyRange: type mismatch")
	assert(srcStart+count <= src.nCells(), "copyRange: out of bounds")
	if count == 0 {
		return
	}
	if src.ntype() == nodeLeaf {
		copyRangeLeaf(dst.data(), src.data(), srcStart, count)
	} else {
		copyRangeInternal(dst.data(), src.data(), srcStart, count)
	}
}

func copyRangeLeaf(dstData, srcData []byte, srcStart, count uint16) {
	const offsetBase = leafHeaderSize
	dstNCells := binary.LittleEndian.Uint16(dstData[1:3])
	dstFirst := binary.LittleEndian.Uint16(dstData[3:5])
	for i := range count {
		srcIdx := srcStart + i
		srcOffPos := offsetBase + srcIdx*2
		cellPos := binary.LittleEndian.Uint16(srcData[srcOffPos : srcOffPos+2])
		keylen := binary.LittleEndian.Uint16(srcData[cellPos : cellPos+2])
		vallen := binary.LittleEndian.Uint16(srcData[cellPos+2 : cellPos+4])
		cellLen := leafCellHeader + keylen
		if srcData[cellPos+4] == 1 {
			cellLen += 8
		} else {
			cellLen += vallen
		}
		newCellPos := dstFirst - cellLen
		copy(dstData[newCellPos:], srcData[cellPos:cellPos+cellLen])
		dstOffPos := offsetBase + dstNCells*2
		binary.LittleEndian.PutUint16(dstData[dstOffPos:dstOffPos+2], newCellPos)
		dstFirst = newCellPos
		dstNCells++
	}
	binary.LittleEndian.PutUint16(dstData[1:3], dstNCells)
	binary.LittleEndian.PutUint16(dstData[3:5], dstFirst)
}

func copyRangeInternal(dstData, srcData []byte, srcStart, count uint16) {
	const offsetBase = internalHeaderSize
	dstNCells := binary.LittleEndian.Uint16(dstData[1:3])
	dstFirst := binary.LittleEndian.Uint16(dstData[3:5])
	for i := uint16(0); i < count; i++ {
		srcIdx := srcStart + i
		srcOffPos := offsetBase + srcIdx*2
		cellPos := binary.LittleEndian.Uint16(srcData[srcOffPos : srcOffPos+2])
		keylen := binary.LittleEndian.Uint16(srcData[cellPos+8 : cellPos+10])
		cellLen := internalCellHeader + keylen
		newCellPos := dstFirst - cellLen
		copy(dstData[newCellPos:], srcData[cellPos:cellPos+cellLen])
		dstOffPos := offsetBase + dstNCells*2
		binary.LittleEndian.PutUint16(dstData[dstOffPos:dstOffPos+2], newCellPos)
		dstFirst = newCellPos
		dstNCells++
	}
	binary.LittleEndian.PutUint16(dstData[1:3], dstNCells)
	binary.LittleEndian.PutUint16(dstData[3:5], dstFirst)
}

func (dst *bnode) copyWithoutCell(src *bnode, removeIdx uint16) {
	assert(src.ntype() == dst.ntype(), "copyWithoutCell: type mismatch")
	assert(removeIdx <= src.nCells(), "copyWithoutCell: index out of bounds")
	dst.copyRange(src, 0, removeIdx)
	dst.copyRange(src, removeIdx+1, src.nCells()-removeIdx-1)
	if dst.ntype() == nodeInternal {
		dst.setRightmostChild(src.rightmostChild())
	}
}

func (n *bnode) appendLeafCellWithOverflow(key, value []byte, overflowID PageId) {
	cellLen := leafCellSize(key, value)
	assert(n.freeSpace() >= 2+cellLen, "appendLeafCellWithOverflow: not enough free space")

	d := n.data()
	newCellPos := n.firstCellOffset() - cellLen
	encodeLeafCell(d[newCellPos:], key, value, overflowID)

	idx := n.nCells()
	offsetPos := n.headerSize() + idx*2
	binary.LittleEndian.PutUint16(d[offsetPos:offsetPos+2], newCellPos)

	n.setFirstCellOffset(newCellPos)
	n.setNCells(idx + 1)
}

func (n *bnode) appendInternalCell(child PageId, key []byte) {
	cellLen := internalCellSize(key)
	assert(n.freeSpace() >= 2+cellLen, "appendInternalCell: not enough free space")

	d := n.data()
	newCellPos := n.firstCellOffset() - cellLen
	encodeInternalCell(d[newCellPos:], child, key)

	idx := n.nCells()
	offsetPos := n.headerSize() + idx*2
	binary.LittleEndian.PutUint16(d[offsetPos:offsetPos+2], newCellPos)

	n.setFirstCellOffset(newCellPos)
	n.setNCells(idx + 1)
}

func (n *bnode) insertInternalCellAt(dstIdx uint16, child PageId, key []byte) {
	nCells := n.nCells()
	assert(dstIdx <= nCells, "insertInternalCellAt: dstIdx out of bounds")

	cellLen := internalCellSize(key)
	assert(n.freeSpace() >= 2+cellLen, "insertInternalCellAt: not enough free space")

	d := n.data()
	newCellPos := n.firstCellOffset() - cellLen
	encodeInternalCell(d[newCellPos:], child, key)

	offsetPos := n.headerSize() + dstIdx*2
	if dstIdx < nCells {
		tail := d[offsetPos : n.headerSize()+nCells*2]
		copy(d[offsetPos+2:], tail)
	}
	binary.LittleEndian.PutUint16(d[offsetPos:offsetPos+2], newCellPos)

	n.setFirstCellOffset(newCellPos)
	n.setNCells(nCells + 1)
}

func encodeInternalCell(buf []byte, child PageId, key []byte) {
	binary.LittleEndian.PutUint64(buf[0:8], uint64(child))
	binary.LittleEndian.PutUint16(buf[8:10], uint16(len(key)))
	copy(buf[internalCellHeader:], key)
}

func encodeLeafCell(buf []byte, key, value []byte, overflowID PageId) {
	binary.LittleEndian.PutUint16(buf[0:2], uint16(len(key)))
	binary.LittleEndian.PutUint16(buf[2:4], uint16(len(value)))
	if overflowID == 0 {
		buf[4] = 0
		copy(buf[leafCellHeader:], key)
		copy(buf[leafCellHeader+len(key):], value)
		return
	}
	buf[4] = 1
	copy(buf[leafCellHeader:], key)
	binary.LittleEndian.PutUint64(buf[leafCellHeader+len(key):], uint64(overflowID))
}

func (n *bnode) leafCellOverflowId(idx uint16) PageId {
	pos := n.cellOffset(idx)
	d := n.data()
	if d[pos+4] != 1 {
		return 0
	}
	keylen := binary.LittleEndian.Uint16(d[pos : pos+2])
	return PageId(binary.LittleEndian.Uint64(d[pos+leafCellHeader+keylen:]))
}

func assert(cond bool, msg string) {
	if !cond {
		panic("btree: " + msg)
	}
}
