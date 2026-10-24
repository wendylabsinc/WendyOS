package backend

import (
	"errors"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func (b *Backend) AudioDevices(session uint64) ([]*pb.AudioDevice, error) {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return nil, errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	response, err := pb.NewWendyAudioServiceClient(conn.Conn).ListAudioDevices(ctx, &pb.ListAudioDevicesRequest{})
	if err != nil {
		return nil, err
	}
	return response.GetDevices(), nil
}
func (b *Backend) SetDefaultAudio(session uint64, id uint32, direction int32) error {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	if id == 0 || (direction != 1 && direction != 2) {
		return errors.New("choose an audio input or output")
	}
	client := pb.NewWendyAudioServiceClient(conn.Conn)
	devices, err := client.ListAudioDevices(ctx, &pb.ListAudioDevicesRequest{})
	if err != nil {
		return err
	}
	found := false
	for _, device := range devices.GetDevices() {
		if device.GetDeviceId() == id && int32(device.GetType()) == direction {
			found = true
			break
		}
	}
	if !found {
		return errors.New("this audio device is no longer available; refresh the device list")
	}
	response, err := client.SetDefaultAudioDevice(ctx, &pb.SetDefaultAudioDeviceRequest{DeviceId: id})
	if err != nil {
		return err
	}
	if !response.GetSuccess() {
		detail := response.GetErrorMessage()
		if detail == "" {
			detail = "the device could not change its default audio device"
		}
		return errors.New(detail)
	}
	return nil
}
