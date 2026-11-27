package data

// receiptHistory is a min-heap of in-window byte-budget losses. App receipts
// are sampled before locking, and independent stream commits may finish out of
// order, so the oldest receipt is not necessarily the first one inserted.
// Keeping the minimum at the root avoids rescanning the full five-minute loss
// history on every incoming record. The retained representation remains one
// int64 per loss; episode loss queries can iterate it in any order.
type receiptHistory []int64

func (h *receiptHistory) add(receipt int64) {
	*h = append(*h, receipt)
	i := len(*h) - 1
	for i > 0 {
		parent := (i - 1) / 2
		if (*h)[parent] <= receipt {
			break
		}
		(*h)[i] = (*h)[parent]
		i = parent
	}
	(*h)[i] = receipt
}

func (h *receiptHistory) expireBefore(cutoff int64) {
	removed := 0
	for len(*h) > 0 && (*h)[0] < cutoff {
		// A pause in ingestion can expire most of the history at once. Do
		// bounded heap work before switching to linear pruning, rather than
		// holding the manager lock for O(N log N) individual removals.
		if removed == 32 {
			kept := (*h)[:0]
			for _, receipt := range *h {
				if receipt >= cutoff {
					kept = append(kept, receipt)
				}
			}
			*h = kept
			for i := len(*h)/2 - 1; i >= 0; i-- {
				h.down(i)
			}
			return
		}
		last := len(*h) - 1
		receipt := (*h)[last]
		*h = (*h)[:last]
		if last == 0 {
			break
		}
		(*h)[0] = receipt
		h.down(0)
		removed++
	}
}

func (h receiptHistory) down(i int) {
	receipt := h[i]
	for {
		child := i*2 + 1
		if child >= len(h) {
			break
		}
		if child+1 < len(h) && h[child+1] < h[child] {
			child++
		}
		if receipt <= h[child] {
			break
		}
		h[i] = h[child]
		i = child
	}
	h[i] = receipt
}
