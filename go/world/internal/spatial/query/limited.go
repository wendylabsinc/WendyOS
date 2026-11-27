package query

// retainBest keeps a max heap of at most limit candidates. A limited query
// still evaluates every candidate and counts all matches, but need not retain
// or sort the rows it will discard. The root is the worst retained row under
// the complete query ordering, including deterministic tie breaks.
func retainBest(found []cand, candidate cand, limit int, compare func(cand, cand) int) []cand {
	if len(found) < limit {
		found = append(found, candidate)
		for child := len(found) - 1; child > 0; {
			parent := (child - 1) / 2
			if compare(found[parent], found[child]) >= 0 {
				break
			}
			found[parent], found[child] = found[child], found[parent]
			child = parent
		}
		return found
	}
	if compare(candidate, found[0]) >= 0 {
		return found
	}
	found[0] = candidate
	for parent := 0; ; {
		child := parent*2 + 1
		if child >= len(found) {
			break
		}
		if child+1 < len(found) && compare(found[child+1], found[child]) > 0 {
			child++
		}
		if compare(found[parent], found[child]) >= 0 {
			break
		}
		found[parent], found[child] = found[child], found[parent]
		parent = child
	}
	return found
}
