package servicejournal

import (
	"context"
	"errors"
	"log"
	"runtime"

	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

// Called under the existing journal lock, only on a fault transition. The
// diagnostic contains code locations and fixed classes, never proof bytes,
// source identities, paths or the error's free-form text.
func (j *Journal) observeFault(err error) {
	if j.faulted {
		return
	}
	caller := "unknown"
	if pc, _, _, ok := runtime.Caller(2); ok {
		if function := runtime.FuncForPC(pc); function != nil {
			caller = function.Name()
		}
	}
	log.Printf("World publication journal fault: caller=%s expired=%t denied=%t clock=%t canceled=%t deadline=%t corrupt=%t",
		caller, errors.Is(err, serviceorigin.ErrExpired), errors.Is(err, serviceorigin.ErrDenied),
		errors.Is(err, serviceorigin.ErrClock), errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrCorrupt))
}
