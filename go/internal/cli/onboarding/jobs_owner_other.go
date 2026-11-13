//go:build !darwin && !linux

package onboarding

import "os"

func preserveJobOwner(_ *os.File, _ string) error { return nil }
