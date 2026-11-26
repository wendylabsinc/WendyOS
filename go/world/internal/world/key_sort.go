package world

import "sort"

// Keep the established JSON-key ordering (including JSON escaping), while
// encoding each key once instead of twice for every sort comparison.
func sortByKey[T any](values []T, key func(T) Key) {
	if len(values) < 2 {
		return
	}
	s := keyedValues[T]{values: values, keys: make([]string, len(values))}
	for i, value := range values {
		s.keys[i] = keyString(key(value))
	}
	sort.Sort(s)
}

type keyedValues[T any] struct {
	values []T
	keys   []string
}

func (s keyedValues[T]) Len() int           { return len(s.values) }
func (s keyedValues[T]) Less(i, j int) bool { return s.keys[i] < s.keys[j] }
func (s keyedValues[T]) Swap(i, j int) {
	s.keys[i], s.keys[j] = s.keys[j], s.keys[i]
	s.values[i], s.values[j] = s.values[j], s.values[i]
}
