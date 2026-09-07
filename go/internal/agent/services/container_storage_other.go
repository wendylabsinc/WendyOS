//go:build !linux

package services

func containerStorageUsage() (partitionUsage, bool) { return partitionUsage{}, false }
