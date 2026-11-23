package rtps

// Discovery is reliable even though our image subscriptions are best effort.
// A maximum sequence number is not an acknowledgement of earlier announcements:
// UDP delivery and late-join history replay can both arrive out of order.
type sequenceRange struct{ first, last SequenceNumber }

type discoverySequences struct {
	ranges []sequenceRange // sorted, disjoint, non-adjacent received/GAP ranges
}

// Bound bookkeeping for a peer sending arbitrarily sparse sequence numbers.
// Forgetting the highest ranges is safe: they can be requested again, while
// recovery of the earliest hole still makes progress.
const maxDiscoveryRanges = 256

func (s *discoverySequences) add(first, last SequenceNumber) {
	if first < 1 || last < first {
		return
	}
	i := 0
	for i < len(s.ranges) && s.ranges[i].last < first-1 {
		i++
	}
	j := i
	for j < len(s.ranges) && s.ranges[j].first-1 <= last {
		first = min(first, s.ranges[j].first)
		last = max(last, s.ranges[j].last)
		j++
	}
	merged := sequenceRange{first, last}
	if i == j {
		s.ranges = append(s.ranges, sequenceRange{})
		copy(s.ranges[i+1:], s.ranges[i:])
	} else {
		copy(s.ranges[i+1:], s.ranges[j:])
		s.ranges = s.ranges[:len(s.ranges)-(j-i)+1]
	}
	s.ranges[i] = merged
	if len(s.ranges) > maxDiscoveryRanges {
		s.ranges = s.ranges[:maxDiscoveryRanges]
	}
}

func (s *discoverySequences) contains(sn SequenceNumber) bool {
	for _, r := range s.ranges {
		if sn < r.first {
			return false
		}
		if sn <= r.last {
			return true
		}
	}
	return false
}

func (s *discoverySequences) firstMissing(first, last SequenceNumber) (SequenceNumber, bool) {
	for _, r := range s.ranges {
		if r.first > first {
			break
		}
		if r.last >= first {
			if r.last >= last {
				return 0, false
			}
			first = r.last + 1
		}
	}
	return first, first <= last
}
