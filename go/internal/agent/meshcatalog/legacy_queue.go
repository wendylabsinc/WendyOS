package meshcatalog

import (
	"encoding/json"
	"errors"
)

const (
	maxLegacyQueueFrames = 16
	maxLegacyQueueBytes  = 128 << 10
)

type legacyQueuedMessage struct {
	message Message
	bytes   int
}

// legacyQueue bounds retained outbound frames on negotiated /1 and /2 links.
// Admission is atomic: an identity bundle and its record are never partially
// admitted. Overload closes the session; reconnect uses fresh synchronizer
// stamps and the catalog's signed records, including withdrawal tombstones.
type legacyQueue struct {
	messages []legacyQueuedMessage
	bytes    int
}

func (q *legacyQueue) add(messages ...Message) error {
	if len(messages) > maxLegacyQueueFrames-len(q.messages) {
		return errors.New("legacy catalog transmit frame queue full")
	}
	batch := make([]legacyQueuedMessage, 0, len(messages))
	total := 0
	for _, message := range messages {
		if err := message.validate(); err != nil {
			return err
		}
		data, err := json.Marshal(message)
		if err != nil {
			return err
		}
		if len(data) == 0 || len(data) > MaxMessageBytes {
			return errors.New("oversized legacy catalog transmit message")
		}
		size := len(data) + 4 // the same length prefix as WriteMessage
		total += size
		if total > maxLegacyQueueBytes-q.bytes {
			return errors.New("legacy catalog transmit byte queue full")
		}
		batch = append(batch, legacyQueuedMessage{message: message, bytes: size})
	}
	q.messages = append(q.messages, batch...)
	q.bytes += total
	return nil
}

func (q *legacyQueue) pop() {
	q.bytes -= q.messages[0].bytes
	q.messages[0] = legacyQueuedMessage{}
	q.messages = q.messages[1:]
	if len(q.messages) == 0 {
		q.messages = nil
	}
}
