package frostfire

import (
	"bytes"
	"errors"
)

type Mode uint8

const (
	ModeUpsert Mode = iota
	ModeInsert
	ModeUpdate
)

var (
	ErrKeyExists   = errors.New("frostfire: key already exists")
	ErrKeyNotFound = errors.New("frostfire: key not found")
)

func (t *BTree) Update(key, value []byte, mode Mode) (PageId, error) {
	assert(len(key) <= maxKeySize, "key too large")

	if t.root == 0 {
		if mode == ModeUpdate {
			return 0, ErrKeyNotFound
		}
		leaf, err := t.allocLeaf()
		if err != nil {
			return 0, err
		}
		if err := t.appendLeafCell(leaf, key, value); err != nil {
			leaf.Unpin()
			t.txn.FreePage(leaf.Id())
			return 0, err
		}
		leaf.Unpin()
		t.root = leaf.Id()
		return leaf.Id(), nil
	}

	rootNode, err := t.get(t.root)
	if err != nil {
		return 0, err
	}

	newChild, extraChild, pushUpKey, err := t.updateNode(rootNode, key, value, mode)
	rootNode.Unpin()
	if err != nil {
		return 0, err
	}

	if newChild == nil {
		return t.root, nil
	}

	t.txn.FreePage(t.root)

	if extraChild == nil {
		newID := newChild.Id()
		newChild.Unpin()
		t.root = newID
		return newID, nil
	}

	newRoot, err := t.allocInternal()
	if err != nil {
		newChild.Unpin()
		extraChild.Unpin()
		return 0, err
	}
	newRoot.appendInternalCell(newChild.Id(), pushUpKey)
	newRoot.setRightmostChild(extraChild.Id())
	newChild.Unpin()
	extraChild.Unpin()
	newRoot.Unpin()
	t.root = newRoot.Id()
	return newRoot.Id(), nil
}

func (t *BTree) updateNode(node *bnode, key, value []byte, mode Mode) (*bnode, *bnode, []byte, error) {
	if node.ntype() == nodeLeaf {
		return t.updateLeaf(node, key, value, mode)
	}
	return t.updateInternal(node, key, value, mode)
}

func (t *BTree) updateLeaf(node *bnode, key, value []byte, mode Mode) (*bnode, *bnode, []byte, error) {
	insertIdx, found := searchLeaf(node, key)
	if found {
		return t.replaceLeafValue(node, insertIdx, key, value, mode)
	}
	return t.addLeafValue(node, insertIdx, key, value, mode)
}

func (t *BTree) replaceLeafValue(node *bnode, idx uint16, key, value []byte, mode Mode) (*bnode, *bnode, []byte, error) {
	if mode == ModeInsert {
		return nil, nil, nil, ErrKeyExists
	}

	// Replacing an overflow value must release the old overflow chain before
	// the leaf is rewritten into a fresh page.
	existing, err := t.leafValue(node, idx)
	if err != nil {
		return nil, nil, nil, err
	}
	if bytes.Equal(existing, value) {
		return nil, nil, nil, nil
	}

	if oldOv := node.leafCellOverflowId(idx); oldOv != 0 {
		if err := t.freeOverflowPages(oldOv); err != nil {
			return nil, nil, nil, err
		}
	}

	newCellSize := leafCellSize(key, value)
	oldCellSize := node.cellSize(idx)
	var needed uint16
	if newCellSize > oldCellSize {
		needed = newCellSize - oldCellSize
	}
	if needed > node.freeSpace() {
		return t.splitLeaf(node, idx, key, value, true)
	}

	newNode, err := t.allocLeaf()
	if err != nil {
		return nil, nil, nil, err
	}
	newNode.copyRange(node, 0, idx)
	if err := t.appendLeafCell(newNode, key, value); err != nil {
		newNode.Unpin()
		return nil, nil, nil, err
	}
	newNode.copyRange(node, idx+1, node.nCells()-idx-1)
	return newNode, nil, nil, nil
}

func (t *BTree) addLeafValue(node *bnode, idx uint16, key, value []byte, mode Mode) (*bnode, *bnode, []byte, error) {
	if mode == ModeUpdate {
		return nil, nil, nil, ErrKeyNotFound
	}

	newCellSize := leafCellSize(key, value)
	if newCellSize+2 > node.freeSpace() {
		return t.splitLeaf(node, idx, key, value, false)
	}

	newNode, err := t.allocLeaf()
	if err != nil {
		return nil, nil, nil, err
	}
	newNode.copyRange(node, 0, idx)
	if err := t.appendLeafCell(newNode, key, value); err != nil {
		newNode.Unpin()
		return nil, nil, nil, err
	}
	newNode.copyRange(node, idx, node.nCells()-idx)
	return newNode, nil, nil, nil
}

func (t *BTree) updateInternal(node *bnode, key, value []byte, mode Mode) (*bnode, *bnode, []byte, error) {
	childIdx := searchInternal(node, key)
	childPageID := node.child(childIdx)
	childNode, err := t.get(childPageID)
	if err != nil {
		return nil, nil, nil, err
	}

	newChild, extraChild, pushUpKey, err := t.updateNode(childNode, key, value, mode)
	childNode.Unpin()
	if err != nil {
		return nil, nil, nil, err
	}
	if newChild == nil {
		return nil, nil, nil, nil
	}

	t.txn.FreePage(childPageID)

	if extraChild == nil {
		return t.replaceInternalChild(node, childIdx, newChild)
	}
	return t.updateInternalAfterChildSplit(node, childIdx, newChild, pushUpKey, extraChild)
}

func (t *BTree) replaceInternalChild(node *bnode, childIdx uint16, newChild *bnode) (*bnode, *bnode, []byte, error) {
	newNode, err := t.allocInternal()
	if err != nil {
		newChild.Unpin()
		return nil, nil, nil, err
	}
	copy(newNode.data(), node.data())
	newNode.setChild(childIdx, newChild.Id())
	newChild.Unpin()
	return newNode, nil, nil, nil
}

func (t *BTree) updateInternalAfterChildSplit(node *bnode, childIdx uint16, newChild *bnode, pushUpKey []byte, extraChild *bnode) (*bnode, *bnode, []byte, error) {
	// A child split produces newChild | pushUpKey | extraChild. If this node
	// has no room for that separator, split this node too.
	newCellSize := internalCellSize(pushUpKey)
	if newCellSize+2 > node.freeSpace() {
		out, extra, sepKey, err := t.splitInternal(node, childIdx, newChild.Id(), pushUpKey, extraChild.Id())
		newChild.Unpin()
		extraChild.Unpin()
		return out, extra, sepKey, err
	}
	out, err := t.applyChildSplitToInternal(node, childIdx, newChild.Id(), pushUpKey, extraChild.Id())
	newChild.Unpin()
	extraChild.Unpin()
	return out, nil, nil, err
}

func (t *BTree) applyChildSplitToInternal(node *bnode, childIdx uint16, newChild PageId, pushUpKey []byte, extraChild PageId) (*bnode, error) {
	newNode, err := t.allocInternal()
	if err != nil {
		return nil, err
	}
	nCells := node.nCells()

	if childIdx < nCells {
		newNode.copyRange(node, 0, childIdx)
		newNode.appendInternalCell(newChild, pushUpKey)
		newNode.appendInternalCell(extraChild, node.key(childIdx))
		newNode.copyRange(node, childIdx+1, nCells-childIdx-1)
		newNode.setRightmostChild(node.rightmostChild())
	} else {
		newNode.copyRange(node, 0, nCells)
		newNode.appendInternalCell(newChild, pushUpKey)
		newNode.setRightmostChild(extraChild)
	}
	return newNode, nil
}

func (t *BTree) splitLeaf(node *bnode, insertIdx uint16, key, value []byte, replacing bool) (*bnode, *bnode, []byte, error) {
	nodeData := node.data()
	totalCells := node.nCells() + 1
	newCellSize := leafCellSize(key, value)
	contentDelta := 2 + newCellSize

	if replacing {
		totalCells = node.nCells()
		contentDelta = newCellSize - leafCellSizeAt(nodeData, insertIdx)
	}

	totalSize := node.totalCellsSize() + contentDelta + node.nCells()*2
	target := totalSize / 2
	splitIdx := leafSplitIndex(nodeData, insertIdx, totalCells, newCellSize, target, replacing)

	overflowID, err := t.leafCellOverflow(key, value)
	if err != nil {
		return nil, nil, nil, err
	}

	node1, err := t.allocLeaf()
	if err != nil {
		return nil, nil, nil, err
	}
	node2, err := t.allocLeaf()
	if err != nil {
		t.txn.FreePage(node1.Id())
		node1.Unpin()
		return nil, nil, nil, err
	}

	if insertIdx < splitIdx {
		node1.copyRange(node, 0, insertIdx)
		node1.appendLeafCellWithOverflow(key, value, overflowID)
		if replacing {
			node1.copyRange(node, insertIdx+1, splitIdx-insertIdx-1)
			node2.copyRange(node, splitIdx, node.nCells()-splitIdx)
		} else {
			node1.copyRange(node, insertIdx, splitIdx-insertIdx-1)
			node2.copyRange(node, splitIdx-1, node.nCells()-(splitIdx-1))
		}
	} else {
		node1.copyRange(node, 0, splitIdx)
		node2.copyRange(node, splitIdx, insertIdx-splitIdx)
		node2.appendLeafCellWithOverflow(key, value, overflowID)
		if replacing {
			node2.copyRange(node, insertIdx+1, node.nCells()-insertIdx-1)
		} else {
			node2.copyRange(node, insertIdx, node.nCells()-insertIdx)
		}
	}

	sepKey := append([]byte(nil), node2.key(0)...)
	return node1, node2, sepKey, nil
}

func leafSplitIndex(nodeData []byte, insertIdx, totalCells, newCellSize, target uint16, replacing bool) uint16 {
	var running uint16
	splitIdx := uint16(1)

	for idx := range totalCells {
		var size uint16
		if idx == insertIdx {
			size = newCellSize
		} else {
			src := idx
			if !replacing && idx > insertIdx {
				src = idx - 1
			}
			size = leafCellSizeAt(nodeData, src)
		}
		running += size + 2
		if running > target {
			splitIdx = idx + 1
			break
		}
	}
	if splitIdx >= totalCells {
		return totalCells - 1
	}
	return splitIdx
}

func (t *BTree) splitInternal(node *bnode, childIdx uint16, newChild PageId, pushUpKey []byte, extraChild PageId) (*bnode, *bnode, []byte, error) {
	nodeData := node.data()
	nCells := node.nCells()
	totalCells := nCells + 1
	newCellSize := internalCellSize(pushUpKey)

	totalSize := node.totalCellsSize() + newCellSize + nCells*2 + 2
	target := totalSize / 2
	splitIdx := internalSplitIndex(nodeData, childIdx, nCells, totalCells, newCellSize, target)

	node1, err := t.allocInternal()
	if err != nil {
		return nil, nil, nil, err
	}
	node2, err := t.allocInternal()
	if err != nil {
		t.txn.FreePage(node1.Id())
		node1.Unpin()
		return nil, nil, nil, err
	}

	var sepKey []byte
	switch {
	case splitIdx == childIdx+1:
		// The new separator stays in node1; the next old separator is promoted.
		node1.copyRange(node, 0, childIdx)
		node1.appendInternalCell(newChild, pushUpKey)
		node1.setRightmostChild(extraChild)

		sepKey = append([]byte(nil), node.key(childIdx)...)

		node2.copyRange(node, childIdx+1, nCells-childIdx-1)
		node2.setRightmostChild(node.rightmostChild())

	case childIdx < splitIdx:
		// The child split lands in node1, but an existing separator later in
		// the original node is promoted.
		node1.copyRange(node, 0, childIdx)
		node1.appendInternalCell(newChild, pushUpKey)
		node1.appendInternalCell(extraChild, node.key(childIdx))
		node1.copyRange(node, childIdx+1, splitIdx-childIdx-2)
		node1.setRightmostChild(node.child(splitIdx - 1))

		sepKey = append([]byte(nil), node.key(splitIdx-1)...)

		node2.copyRange(node, splitIdx, nCells-splitIdx)
		node2.setRightmostChild(node.rightmostChild())

	case childIdx == splitIdx:
		// The new separator itself is promoted, so newChild becomes node1's
		// rightmost child and extraChild starts node2.
		node1.copyRange(node, 0, childIdx)
		node1.setRightmostChild(newChild)

		sepKey = append([]byte(nil), pushUpKey...)

		if childIdx < nCells {
			node2.appendInternalCell(extraChild, node.key(childIdx))
			node2.copyRange(node, childIdx+1, nCells-childIdx-1)
			node2.setRightmostChild(node.rightmostChild())
		} else {
			node2.setRightmostChild(extraChild)
		}

	default:
		// The promoted separator comes before the child split; the split pair
		// is rebuilt inside node2.
		node1.copyRange(node, 0, splitIdx)
		node1.setRightmostChild(node.child(splitIdx))

		sepKey = append([]byte(nil), node.key(splitIdx)...)

		node2.copyRange(node, splitIdx+1, childIdx-splitIdx-1)
		node2.appendInternalCell(newChild, pushUpKey)
		if childIdx < nCells {
			node2.appendInternalCell(extraChild, node.key(childIdx))
			node2.copyRange(node, childIdx+1, nCells-childIdx-1)
			node2.setRightmostChild(node.rightmostChild())
		} else {
			node2.setRightmostChild(extraChild)
		}
	}

	return node1, node2, sepKey, nil
}

func internalSplitIndex(nodeData []byte, childIdx, nCells, totalCells, newCellSize, target uint16) uint16 {
	var running uint16
	splitIdx := uint16(1)

	for i := range totalCells {
		var size uint16
		switch {
		case i < childIdx:
			size = internalCellSizeAt(nodeData, i)
		case i == childIdx:
			size = newCellSize
		case i == childIdx+1 && childIdx < nCells:
			size = internalCellSizeAt(nodeData, childIdx)
		default:
			size = internalCellSizeAt(nodeData, i-1)
		}
		running += size + 2
		if running > target {
			splitIdx = i
			break
		}
	}
	if splitIdx >= totalCells {
		return totalCells - 1
	}
	return splitIdx
}

func (t *BTree) appendLeafCell(n *bnode, key, value []byte) error {
	overflowID, err := t.leafCellOverflow(key, value)
	if err != nil {
		return err
	}
	n.appendLeafCellWithOverflow(key, value, overflowID)
	return nil
}
