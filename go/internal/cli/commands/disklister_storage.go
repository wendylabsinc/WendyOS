package commands

// StorageType identifies the underlying storage protocol of a drive.
type StorageType int

const (
	StorageUnknown StorageType = iota
	StorageNVMe
)
