package commands

import "time"

const initialRefreshInterval = 500 * time.Millisecond

type increasingRefreshInterval struct {
	next time.Duration
}

func (i *increasingRefreshInterval) delay(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	if i.next <= 0 {
		i.next = initialRefreshInterval
	}

	delay := i.next
	if delay > max {
		delay = max
	}

	i.next *= 2
	if i.next > max {
		i.next = max
	}
	return delay
}
