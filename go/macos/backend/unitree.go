package backend

import (
	"context"
	"errors"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/unitreecloud"
	"github.com/wendylabsinc/wendy/go/internal/shared/unitree"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// UnitreeRobot deliberately excludes robot keys and account credentials.
type UnitreeRobot struct {
	Serial, Name, Address string
	Paired, Connected     bool
}
type UnitreeSnapshot struct {
	Robots  []UnitreeRobot
	Warning string
}

func (b *Backend) UnitreePairings(session uint64) (UnitreeSnapshot, error) {
	if session == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		devices, err := unitree.Discover(ctx)
		result := UnitreeSnapshot{}
		for _, d := range devices {
			result.Robots = append(result.Robots, UnitreeRobot{Serial: d.Serial, Name: d.Name, Address: d.Address})
		}
		return result, err
	}
	ctx, done := b.begin()
	defer done()
	conn, err := b.connection(session)
	if err != nil {
		return UnitreeSnapshot{}, err
	}
	listed, err := conn.UnitreeService.ListUnitreePairings(ctx, &pb.ListUnitreePairingsRequest{})
	if err != nil {
		return UnitreeSnapshot{}, err
	}
	byID := map[string]UnitreeRobot{}
	var serials []string
	for _, d := range listed.GetRobots() {
		if d == nil {
			continue
		}
		byID[d.Serial] = UnitreeRobot{d.Serial, d.Name, d.Address, d.Paired, d.Connected}
		serials = append(serials, d.Serial)
	}
	found, scanErr := conn.UnitreeService.DiscoverUnitreeRobots(ctx, &pb.DiscoverUnitreeRobotsRequest{KnownSerials: serials})
	for _, d := range found.GetRobots() {
		if d == nil {
			continue
		}
		old, ok := byID[d.Serial]
		if !ok {
			old = UnitreeRobot{Serial: d.Serial, Name: d.Name}
		}
		old.Address = d.Address
		byID[d.Serial] = old
	}
	result := UnitreeSnapshot{}
	if scanErr != nil {
		result.Warning = "LAN discovery failed. Saved pairings are shown; you can also enter a robot's IP address."
	}
	for _, d := range byID {
		result.Robots = append(result.Robots, d)
	}
	sort.Slice(result.Robots, func(i, j int) bool { return result.Robots[i].Serial < result.Robots[j].Serial })
	return result, nil
}

// Login is scoped to one requested pairing. Secrets never cross the C bridge
// on the way back, and direct Mac connections keep no credentials on disk.
func unitreeKey(ctx context.Context, serial, region, email, password string) (string, bool, error) {
	if email == "" && password == "" {
		return "", false, nil
	}
	client, err := unitreecloud.NewClient(region)
	if err != nil {
		return "", false, err
	}
	account, err := client.Login(ctx, email, password)
	if err != nil {
		return "", false, errors.New(unitreecloud.LoginFailureMessage(err))
	}
	for _, d := range account.Devices {
		if d.Serial == serial {
			return d.EncryptionKey, true, nil
		}
	}
	return "", false, errors.New("this robot is not bound to the Unitree account; bind it in the Unitree Go app first")
}

func validateUnitreeTarget(serial, address string) error {
	ip := net.ParseIP(address)
	if !unitree.ValidSerial(serial) || ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || (!ip.IsGlobalUnicast() && !ip.IsLinkLocalUnicast()) {
		return errors.New("enter the robot's serial number and unicast IPv4 address")
	}
	return nil
}
func (b *Backend) PairUnitree(session uint64, serial, name, address, region, email, password string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return b.PairUnitreeContext(ctx, session, serial, name, address, region, email, password)
}

func (b *Backend) PairUnitreeContext(ctx context.Context, session uint64, serial, name, address, region, email, password string) error {
	if session == 0 {
		return errors.New("select a Wendy device before pairing")
	}
	if err := validateUnitreeTarget(serial, address); err != nil {
		return err
	}
	// Other desktop operations can hold the connection lock. Keep waiting for
	// that lock cancellable as well as the login and pairing requests.
	if err := b.lockUnitreeContext(ctx); err != nil {
		return err
	}
	defer b.mu.Unlock()
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	if !conn.IsMTLS && !conn.IsSessionProxy && !strings.HasPrefix(conn.Host, "unix:") {
		return errors.New("Unitree pairing requires an authenticated Wendy device connection")
	}
	key, account, err := unitreeKey(ctx, serial, region, email, password)
	if err != nil {
		return err
	}
	req := &pb.PairUnitreeRequest{Serial: serial, Name: name, Address: address, EncryptionKey: key}
	if account && key == "" {
		req.Token = unitree.AccountFlowMarker
	}
	response, err := conn.UnitreeService.PairUnitree(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			return errors.New("Go2 pairing timed out or was canceled. Check the Wendy device connection and retry.")
		}
		return errors.New("Unitree pairing failed. Check the robot address, sign in to Unitree, and verify the Wendy agent is up to date.")
	}
	if response.GetRobot() == nil || !response.GetRobot().GetPaired() {
		return errors.New("the Wendy device did not confirm the Unitree pairing")
	}
	return nil
}

func (b *Backend) lockUnitreeContext(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if b.mu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (b *Backend) ForgetUnitree(session uint64, serial string) error {
	if session == 0 {
		return errors.New("select a Wendy device before forgetting a pairing")
	}
	ctx, done := b.begin()
	defer done()
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	_, err = conn.UnitreeService.ForgetUnitree(ctx, &pb.ForgetUnitreeRequest{Serial: serial})
	return err
}

type unitreeControlLink interface {
	Move(context.Context, float64, float64, float64) error
	Stop(context.Context) error
	Close() error
	Connected() bool
}

// A separate controller keeps discovery, login, and agent RPCs off the motion
// path. Handles prevent a disappearing view from stopping a newer connection.
type UnitreeController struct {
	closed             bool
	mu                 sync.Mutex
	link               unitreeControlLink
	handle, generation uint64
	timer              *time.Timer
	lease              time.Duration
	videoFrames        <-chan unitree.VideoFrame
	videoCancel        func()
}

func (c *UnitreeController) Connect(ctx context.Context, serial, address, region, email, password string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := validateUnitreeTarget(serial, address); err != nil {
		return 0, err
	}
	key, signedIn, err := unitreeKey(ctx, serial, region, email, password)
	if err != nil {
		return 0, err
	}
	link, err := unitree.Connect(ctx, address, "", key)
	if err != nil {
		message := unitree.ConnectionFailureMessage(err)
		if signedIn {
			message = "Unitree sign-in succeeded. " + message
		}
		return 0, errors.New(message)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		_ = link.Close()
		return 0, err
	}
	if c.closed {
		_ = link.Close()
		return 0, errors.New("the app is shutting down")
	}
	c.closeLocked()
	c.handle++
	c.link = link
	return c.handle, nil
}
func (c *UnitreeController) cancelLeaseLocked() {
	c.generation++
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
}
func (c *UnitreeController) stopLocked() error {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	return c.link.Stop(ctx)
}
func (c *UnitreeController) closeLocked() {
	c.cancelLeaseLocked()
	c.stopVideoLocked()
	if c.link != nil {
		_ = c.stopLocked()
		_ = c.link.Close()
		c.link = nil
	}
}
func (c *UnitreeController) Close(handle uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if handle != c.handle || c.link == nil {
		return nil
	}
	c.cancelLeaseLocked()
	err := c.stopLocked()
	c.stopVideoLocked()
	_ = c.link.Close()
	c.link = nil
	return err
}
func (c *UnitreeController) Move(handle uint64, forward, lateral, yaw float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if handle == 0 || handle != c.handle || c.link == nil {
		return errors.New("connect to the Go2 before using remote control")
	}
	c.cancelLeaseLocked()
	if !unitree.ValidMotion(forward, lateral, yaw) {
		_ = c.stopLocked()
		return errors.New("Go2 velocity outside limits")
	}
	if forward == 0 && lateral == 0 && yaw == 0 {
		return c.stopLocked()
	}
	if !c.link.Connected() {
		_ = c.stopLocked()
		return errors.New("Go2 disconnected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	err := c.link.Move(ctx, forward, lateral, yaw)
	cancel()
	if err != nil {
		_ = c.stopLocked()
		return errors.New("Could not send movement to Go2; Stop was attempted. Check the robot connection.")
	}
	generation := c.generation
	lease := c.lease
	if lease == 0 {
		lease = 500 * time.Millisecond
	}
	c.timer = time.AfterFunc(lease, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.handle == handle && c.generation == generation && c.link != nil {
			_ = c.stopLocked()
		}
	})
	return nil
}

// Shutdown also rejects a connection whose dial finishes after app termination starts.
func (c *UnitreeController) Shutdown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.closeLocked()
}
