//go:build android

package central

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/androidhost"
	"strings"
	"sync"
)

type Connection struct {
	id   int64
	once sync.Once
}

func Connect(address string, timeoutSeconds int) (*Connection, error) {
	var id int64
	err := androidhost.Call("ble.connect", map[string]any{"address": address, "timeout": timeoutSeconds}, &id)
	if err != nil {
		return nil, err
	}
	return &Connection{id: id}, nil
}
func (c *Connection) call(method string, args map[string]any, out any) error {
	if args == nil {
		args = map[string]any{}
	}
	args["id"] = c.id
	err := androidhost.Call("ble."+method, args, out)
	if err != nil {
		switch err.Error() {
		case "timeout":
			return ErrRecvTimeout
		case "not_found":
			return ErrGATTNotFound
		case "disconnected":
			return ErrGATTDisconnected
		}
	}
	return err
}
func (c *Connection) DiscoverServices(seconds int) error {
	return c.call("discover", map[string]any{"timeout": seconds}, nil)
}
func (c *Connection) ReadCharacteristic(service, characteristic string) ([]byte, error) {
	var out []byte
	err := c.call("read", map[string]any{"service": service, "characteristic": characteristic}, &out)
	return out, err
}
func (c *Connection) WriteCharacteristic(service, characteristic string, data []byte) error {
	return c.write(service, characteristic, data, false)
}
func (c *Connection) WriteCharacteristicNoResponse(service, characteristic string, data []byte) error {
	return c.write(service, characteristic, data, true)
}
func (c *Connection) write(service, characteristic string, data []byte, noResponse bool) error {
	return c.call("write", map[string]any{"service": service, "characteristic": characteristic, "data": data, "noResponse": noResponse}, nil)
}
func (c *Connection) Subscribe(service, characteristic string) error {
	return c.call("subscribe", map[string]any{"service": service, "characteristic": characteristic}, nil)
}
func (c *Connection) WaitNotification(service, characteristic string, seconds int) ([]byte, error) {
	var out []byte
	err := c.call("notification", map[string]any{"service": service, "characteristic": characteristic, "timeout": seconds}, &out)
	return out, err
}
func (c *Connection) OpenL2CAP(psm uint16, seconds int) error {
	return c.call("l2cap", map[string]any{"psm": psm, "timeout": seconds}, nil)
}
func (c *Connection) L2CAPSend(data []byte) error {
	return c.call("send", map[string]any{"data": data}, nil)
}
func (c *Connection) L2CAPRecv(seconds int) ([]byte, error) {
	var out []byte
	err := c.call("receive", map[string]any{"timeout": seconds}, &out)
	return out, err
}
func (c *Connection) HasService(service string) bool {
	var out bool
	_ = c.call("hasService", map[string]any{"service": service}, &out)
	return out
}
func (c *Connection) ListServices() string {
	var out []string
	_ = c.call("services", nil, &out)
	return strings.Join(out, ",")
}
func (c *Connection) Close() { c.once.Do(func() { _ = c.call("close", nil, nil) }) }
