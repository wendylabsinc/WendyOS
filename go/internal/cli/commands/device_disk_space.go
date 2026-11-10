package commands

import (
	"context"
	"errors"
	"fmt"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// A deploy that fills the device's disk fails while the device unpacks the
// image, and can take the device's own logging and agent down with it
// (WDY-4049). A re-deploy sends only the layers that changed, so a device with
// a few hundred MB free usually still takes a code change: before the build,
// only a disk with almost no room left is refused. Once the build has run and
// the device has said which layers it lacks, deviceImageSpaceError judges the
// actual image.
const (
	deviceMinFreeBytes int64 = 512 << 20 // 512 MiB
	// unpackedLayerFactor estimates the disk a compressed layer takes on the
	// device: the compressed blob in containerd's content store plus its
	// unpacked snapshot, typically two to three times the compressed size.
	unpackedLayerFactor = 3
)

// deviceStorage is the free space on the device filesystem that holds
// container images, and where that filesystem is mounted.
type deviceStorage struct {
	free  int64
	mount string
}

func (s deviceStorage) mountName() string {
	if s.mount == "" {
		return "the disk that holds container images"
	}
	return s.mount
}

// deviceStorageFrom reads the free space on the filesystem the agent reports
// holding its container images. ok is false for an agent that does not report
// it: the root filesystem is no stand-in, since WendyOS keeps images on /data
// and its root slot can be nearly full by design.
func deviceStorageFrom(resp *agentpb.GetAgentVersionResponse) (deviceStorage, bool) {
	p := resp.GetContainerStorage()
	if p == nil || p.GetTotalBytes() <= 0 || p.GetUsedBytes() < 0 || p.GetUsedBytes() > p.GetTotalBytes() {
		return deviceStorage{}, false
	}
	return deviceStorage{free: p.GetTotalBytes() - p.GetUsedBytes(), mount: p.GetMountpoint()}, true
}

func deviceSpaceSteps() []string {
	return []string{
		"Run `wendy device cache prune` to delete container data the device no longer uses.",
		"Remove apps you no longer need from the device: wendy device apps remove <app>",
	}
}

// checkDeviceDiskSpace refuses a deploy before the build when the disk that
// holds the device's container images has too little room for even a small
// change.
func checkDeviceDiskSpace(resp *agentpb.GetAgentVersionResponse) error {
	storage, ok := deviceStorageFrom(resp)
	if !ok || storage.free >= deviceMinFreeBytes {
		return nil
	}
	return withNextSteps(commandErrorf(errDiskSpaceLow,
		"not enough free disk space on the device: %s free on %s, where it keeps container images, and a deploy needs at least %s",
		humanBytes(uint64(storage.free)), storage.mountName(), humanBytes(uint64(deviceMinFreeBytes))), deviceSpaceSteps()...)
}

type deviceStorageKey struct{}

// withDeviceStorage records the device's free image space for the chunk push,
// which checks the layers the device lacks against it. An agent that reports
// no disk usage records nothing, and the push checks nothing.
func withDeviceStorage(ctx context.Context, resp *agentpb.GetAgentVersionResponse) context.Context {
	if storage, ok := deviceStorageFrom(resp); ok {
		return context.WithValue(ctx, deviceStorageKey{}, storage)
	}
	return ctx
}

func deviceStorageFor(ctx context.Context) (deviceStorage, bool) {
	storage, ok := ctx.Value(deviceStorageKey{}).(deviceStorage)
	return storage, ok
}

// deviceImageSpaceError refuses layers whose compressed bytes alone, need, are
// more than the device has free: they cannot fit, whatever they unpack to.
func deviceImageSpaceError(storage deviceStorage, need int64) error {
	if need <= storage.free {
		return nil
	}
	return withNextSteps(commandErrorf(errDiskSpaceLow,
		"not enough free disk space on the device for this image: the layers it lacks are %s compressed, and %s is free on %s",
		humanBytes(uint64(need)), humanBytes(uint64(storage.free)), storage.mountName()), deviceSpaceSteps()...)
}

// deviceImageSpaceWarning returns a warning when layers of need compressed
// bytes fit on the device but may not once unpacked, or "" when they fit.
func deviceImageSpaceWarning(storage deviceStorage, need int64) string {
	if need*unpackedLayerFactor <= storage.free {
		return ""
	}
	return fmt.Sprintf("the device has %s free on %s, and this image's new layers may take up to %s once unpacked. Free space with 'wendy device cache prune'.",
		humanBytes(uint64(storage.free)), storage.mountName(), humanBytes(uint64(need*unpackedLayerFactor)))
}

// chunkFailureRepeatsOnRegistryPush reports whether a failed fast deploy would
// fail the same way as a registry push, so falling back would only repeat it:
// the device lacks room for the image, and a registry push needs at least as
// much, or Docker lacks Buildx and the push would build with Docker too.
func chunkFailureRepeatsOnRegistryPush(err error, builder string) bool {
	return errors.Is(err, errDiskSpaceLow) || (errors.Is(err, errBuildxMissing) && registryPushWouldUseDocker(builder))
}
