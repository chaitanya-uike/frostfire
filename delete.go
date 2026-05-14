package frostfire

func (t *BTree) Delete(key []byte) (PageId, bool, error) {
	if t.root == 0 {
		return 0, false, nil
	}

	rootNode, err := t.get(t.root)
	if err != nil {
		return 0, false, err
	}

	newRoot, found, err := t.delete(rootNode, key)
	rootNode.Unpin()
	if err != nil {
		return 0, false, err
	}
	if !found {
		return t.root, false, nil
	}

	t.txn.FreePage(t.root)

	if newRoot.ntype() == nodeLeaf && newRoot.nCells() == 0 {
		newID := newRoot.Id()
		newRoot.Unpin()
		t.txn.FreePage(newID)
		t.root = 0
		return 0, true, nil
	}

	if newRoot.ntype() == nodeInternal && newRoot.nCells() == 0 {
		newRootID := newRoot.rightmostChild()
		oldID := newRoot.Id()
		newRoot.Unpin()
		t.txn.FreePage(oldID)
		t.root = newRootID
		return newRootID, true, nil
	}

	id := newRoot.Id()
	newRoot.Unpin()
	t.root = id
	return id, true, nil
}

func (t *BTree) delete(node *bnode, key []byte) (*bnode, bool, error) {
	if node.ntype() == nodeLeaf {
		return t.deleteFromLeaf(node, key)
	}
	return t.deleteFromInternal(node, key)
}

func (t *BTree) deleteFromLeaf(node *bnode, key []byte) (*bnode, bool, error) {
	deleteIdx, exists := searchLeaf(node, key)
	if !exists {
		return nil, false, nil
	}

	// Deleting an overflow value must release the old overflow chain.
	if oldOv := node.leafCellOverflowId(deleteIdx); oldOv != 0 {
		if err := t.freeOverflowPages(oldOv); err != nil {
			return nil, false, err
		}
	}

	newNode, err := t.allocLeaf()
	if err != nil {
		return nil, false, err
	}
	newNode.copyWithoutCell(node, deleteIdx)
	return newNode, true, nil
}

func (t *BTree) deleteFromInternal(node *bnode, key []byte) (*bnode, bool, error) {
	childIdx := searchInternal(node, key)
	childPageID := node.child(childIdx)
	childNode, err := t.get(childPageID)
	if err != nil {
		return nil, false, err
	}

	newChild, found, err := t.delete(childNode, key)
	childNode.Unpin()
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}

	t.txn.FreePage(childPageID)

	if !newChild.underfull() {
		return t.replaceChildAfterDelete(node, childIdx, newChild)
	}

	if dst, ok, err := t.borrowForUnderfullChild(node, childIdx, newChild); ok || err != nil {
		return dst, ok, err
	}
	return t.mergeUnderfullChild(node, childIdx, newChild)
}

func (t *BTree) replaceChildAfterDelete(node *bnode, childIdx uint16, newChild *bnode) (*bnode, bool, error) {
	dst, err := t.allocInternal()
	if err != nil {
		newChild.Unpin()
		return nil, false, err
	}
	copy(dst.data(), node.data())
	dst.setChild(childIdx, newChild.Id())
	newChild.Unpin()
	return dst, true, nil
}

func (t *BTree) borrowForUnderfullChild(node *bnode, childIdx uint16, newChild *bnode) (*bnode, bool, error) {
	if childIdx > 0 {
		leftPageID := node.child(childIdx - 1)
		leftSibling, err := t.get(leftPageID)
		if err != nil {
			newChild.Unpin()
			return nil, false, err
		}
		if siblingCanDonate(leftSibling) {
			newLeft, sepKey, err := t.borrowFromLeft(node, childIdx, leftSibling, newChild)
			leftSibling.Unpin()
			if err != nil {
				newChild.Unpin()
				return nil, false, err
			}

			t.txn.FreePage(leftPageID)
			dst, err := t.rebuildParentForBorrow(node, childIdx-1, newLeft.Id(), sepKey, newChild.Id())
			newLeft.Unpin()
			newChild.Unpin()
			if err != nil {
				return nil, false, err
			}
			return dst, true, nil
		}
		leftSibling.Unpin()
	}

	if childIdx < node.nCells() {
		rightPageID := node.child(childIdx + 1)
		rightSibling, err := t.get(rightPageID)
		if err != nil {
			newChild.Unpin()
			return nil, false, err
		}
		if siblingCanDonate(rightSibling) {
			newRight, sepKey, err := t.borrowFromRight(node, childIdx, rightSibling, newChild)
			rightSibling.Unpin()
			if err != nil {
				newChild.Unpin()
				return nil, false, err
			}

			t.txn.FreePage(rightPageID)
			dst, err := t.rebuildParentForBorrow(node, childIdx, newChild.Id(), sepKey, newRight.Id())
			newRight.Unpin()
			newChild.Unpin()
			if err != nil {
				return nil, false, err
			}
			return dst, true, nil
		}
		rightSibling.Unpin()
	}

	return nil, false, nil
}

func (t *BTree) borrowFromLeft(node *bnode, childIdx uint16, leftSibling, child *bnode) (*bnode, []byte, error) {
	if child.ntype() == nodeLeaf {
		return t.borrowFromLeftLeaf(leftSibling, child)
	}
	return t.borrowFromLeftInternal(leftSibling, node.key(childIdx-1), child)
}

func (t *BTree) borrowFromRight(node *bnode, childIdx uint16, rightSibling, child *bnode) (*bnode, []byte, error) {
	if child.ntype() == nodeLeaf {
		return t.borrowFromRightLeaf(rightSibling, child)
	}
	return t.borrowFromRightInternal(rightSibling, node.key(childIdx), child)
}

func (t *BTree) mergeUnderfullChild(node *bnode, childIdx uint16, newChild *bnode) (*bnode, bool, error) {
	// Neither sibling can donate, so combine the child with one sibling and
	// remove their separator from the parent.
	if childIdx > 0 {
		return t.mergeUnderfullChildWithLeft(node, childIdx, newChild)
	}
	if childIdx < node.nCells() {
		return t.mergeUnderfullChildWithRight(node, childIdx, newChild)
	}
	return newChild, true, nil
}

func (t *BTree) mergeUnderfullChildWithLeft(node *bnode, childIdx uint16, newChild *bnode) (*bnode, bool, error) {
	leftPageID := node.child(childIdx - 1)
	leftSibling, err := t.get(leftPageID)
	if err != nil {
		newChild.Unpin()
		return nil, false, err
	}

	merged, err := t.mergeWithLeft(leftSibling, node.key(childIdx-1), newChild)
	leftSibling.Unpin()
	if err != nil {
		newChild.Unpin()
		return nil, false, err
	}

	t.txn.FreePage(leftPageID)
	newChildID := newChild.Id()
	newChild.Unpin()
	t.txn.FreePage(newChildID)

	dst, err := t.rebuildParentForMerge(node, childIdx-1, merged.Id())
	merged.Unpin()
	if err != nil {
		return nil, false, err
	}
	return dst, true, nil
}

func (t *BTree) mergeUnderfullChildWithRight(node *bnode, childIdx uint16, newChild *bnode) (*bnode, bool, error) {
	rightPageID := node.child(childIdx + 1)
	rightSibling, err := t.get(rightPageID)
	if err != nil {
		newChild.Unpin()
		return nil, false, err
	}

	merged, err := t.mergeWithRight(newChild, rightSibling, node.key(childIdx))
	rightSibling.Unpin()
	if err != nil {
		newChild.Unpin()
		return nil, false, err
	}

	t.txn.FreePage(rightPageID)
	newChildID := newChild.Id()
	newChild.Unpin()
	t.txn.FreePage(newChildID)

	dst, err := t.rebuildParentForMerge(node, childIdx, merged.Id())
	merged.Unpin()
	if err != nil {
		return nil, false, err
	}
	return dst, true, nil
}

func siblingCanDonate(sibling *bnode) bool {
	if sibling.nCells() <= minCellsPerPage {
		return false
	}
	borrowSize := sibling.cellSize(sibling.nCells() - 1)
	used := uint16(PageSize) - sibling.freeSpace()
	return used-borrowSize-2 >= PageSize/4
}

// Rebuild the parent after a borrow. sepKey is the new boundary between the
// repaired left and right children.
func (t *BTree) rebuildParentForBorrow(node *bnode, sepIdx uint16, leftChildID PageId, sepKey []byte, rightChildID PageId) (*bnode, error) {
	dst, err := t.allocInternal()
	if err != nil {
		return nil, err
	}
	nCells := node.nCells()
	dst.copyRange(node, 0, sepIdx)
	dst.appendInternalCell(leftChildID, sepKey)
	if sepIdx+1 < nCells {
		dst.appendInternalCell(rightChildID, node.key(sepIdx+1))
		dst.copyRange(node, sepIdx+2, nCells-sepIdx-2)
		dst.setRightmostChild(node.rightmostChild())
	} else {
		dst.setRightmostChild(rightChildID)
	}
	return dst, nil
}

func (t *BTree) borrowFromLeftLeaf(leftSibling, child *bnode) (*bnode, []byte, error) {
	borrowIdx := leftSibling.nCells() - 1
	child.copyCellAt(0, leftSibling, borrowIdx)
	newLeft, err := t.allocLeaf()
	if err != nil {
		return nil, nil, err
	}
	newLeft.copyWithoutCell(leftSibling, borrowIdx)
	sepKey := append([]byte(nil), child.key(0)...)
	return newLeft, sepKey, nil
}

func (t *BTree) borrowFromLeftInternal(leftSibling *bnode, parentSepKey []byte, child *bnode) (*bnode, []byte, error) {
	// Internal borrow rotates through the parent: left's largest separator
	// moves up, and the old parent separator moves down into child.
	borrowIdx := leftSibling.nCells() - 1
	pulledUpKey := append([]byte(nil), leftSibling.key(borrowIdx)...)
	pushedDownChild := leftSibling.rightmostChild()
	newLeftRightmost := leftSibling.child(borrowIdx)

	child.insertInternalCellAt(0, pushedDownChild, parentSepKey)

	newLeft, err := t.allocInternal()
	if err != nil {
		return nil, nil, err
	}
	newLeft.copyRange(leftSibling, 0, borrowIdx)
	newLeft.setRightmostChild(newLeftRightmost)
	return newLeft, pulledUpKey, nil
}

func (t *BTree) borrowFromRightLeaf(rightSibling, child *bnode) (*bnode, []byte, error) {
	child.copyCell(rightSibling, 0)
	newRight, err := t.allocLeaf()
	if err != nil {
		return nil, nil, err
	}
	newRight.copyWithoutCell(rightSibling, 0)
	sepKey := append([]byte(nil), newRight.key(0)...)
	return newRight, sepKey, nil
}

func (t *BTree) borrowFromRightInternal(rightSibling *bnode, parentSepKey []byte, child *bnode) (*bnode, []byte, error) {
	// The old parent separator moves down,
	// and right's smallest separator replaces it in the parent.
	pulledUpKey := append([]byte(nil), rightSibling.key(0)...)
	pushedDownChild := child.rightmostChild()
	newChildRightmost := rightSibling.child(0)

	child.appendInternalCell(pushedDownChild, parentSepKey)
	child.setRightmostChild(newChildRightmost)

	newRight, err := t.allocInternal()
	if err != nil {
		return nil, nil, err
	}
	newRight.copyWithoutCell(rightSibling, 0)
	return newRight, pulledUpKey, nil
}

func (t *BTree) mergeWithLeft(leftSibling *bnode, parentSepKey []byte, child *bnode) (*bnode, error) {
	isLeaf := child.ntype() == nodeLeaf
	var merged *bnode
	var err error
	if isLeaf {
		merged, err = t.allocLeaf()
	} else {
		merged, err = t.allocInternal()
	}
	if err != nil {
		return nil, err
	}

	merged.copyRange(leftSibling, 0, leftSibling.nCells())
	if !isLeaf {
		// Internal merges pull the parent separator down between the two
		// children.
		merged.appendInternalCell(leftSibling.rightmostChild(), parentSepKey)
	}
	merged.copyRange(child, 0, child.nCells())
	if !isLeaf {
		merged.setRightmostChild(child.rightmostChild())
	}
	return merged, nil
}

func (t *BTree) mergeWithRight(child, rightSibling *bnode, parentSepKey []byte) (*bnode, error) {
	isLeaf := child.ntype() == nodeLeaf
	var merged *bnode
	var err error
	if isLeaf {
		merged, err = t.allocLeaf()
	} else {
		merged, err = t.allocInternal()
	}
	if err != nil {
		return nil, err
	}

	merged.copyRange(child, 0, child.nCells())
	if !isLeaf {
		merged.appendInternalCell(child.rightmostChild(), parentSepKey)
	}
	merged.copyRange(rightSibling, 0, rightSibling.nCells())
	if !isLeaf {
		merged.setRightmostChild(rightSibling.rightmostChild())
	}
	return merged, nil
}

func (t *BTree) rebuildParentForMerge(node *bnode, sepIdx uint16, mergedID PageId) (*bnode, error) {
	// Drop sepIdx from the parent and replace the two children around it with
	// the single merged child.
	dst, err := t.allocInternal()
	if err != nil {
		return nil, err
	}
	nCells := node.nCells()
	dst.copyRange(node, 0, sepIdx)
	if sepIdx+1 < nCells {
		dst.appendInternalCell(mergedID, node.key(sepIdx+1))
		dst.copyRange(node, sepIdx+2, nCells-sepIdx-2)
		dst.setRightmostChild(node.rightmostChild())
	} else {
		dst.setRightmostChild(mergedID)
	}
	return dst, nil
}
