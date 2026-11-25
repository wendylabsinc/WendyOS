//go:build !darwin && !linux

package command

import "time"

func annotationBootTime() time.Duration { return -1 }
