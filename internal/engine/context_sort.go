package engine

import (
	"context"
	"fmt"
)

// contextHeapSort sorts values in place without allocating a second O(n)
// buffer. The semantic source path can receive hundreds of thousands of
// derived items, so both cancellation and the absence of an unaccounted sort
// scratch buffer are part of its resource contract.
func contextHeapSort[T any](ctx context.Context, values []T, less func(a, b T) bool) error {
	if ctx == nil {
		return fmt.Errorf("context sort: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	checks := 0
	check := func() error {
		checks++
		if checks&255 == 0 {
			return ctx.Err()
		}
		return nil
	}
	var sift func(root, end int) error
	sift = func(root, end int) error {
		for {
			child := root*2 + 1
			if child >= end {
				return nil
			}
			if err := check(); err != nil {
				return err
			}
			if child+1 < end && less(values[child], values[child+1]) {
				child++
			}
			if !less(values[root], values[child]) {
				return nil
			}
			values[root], values[child] = values[child], values[root]
			root = child
		}
	}
	for root := len(values)/2 - 1; root >= 0; root-- {
		if err := sift(root, len(values)); err != nil {
			return err
		}
	}
	for end := len(values) - 1; end > 0; end-- {
		if err := check(); err != nil {
			return err
		}
		values[0], values[end] = values[end], values[0]
		if err := sift(0, end); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func contextSortStrings(ctx context.Context, values []string) error {
	return contextHeapSort(ctx, values, func(a, b string) bool { return a < b })
}
