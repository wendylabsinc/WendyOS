//go:build !linux

package main

import "context"

func cleanupResetMesh(context.Context) error { return nil }
