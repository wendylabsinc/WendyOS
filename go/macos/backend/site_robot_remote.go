package backend

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
	"net/http"
	"time"
)

type SiteRobot = twinrobot.Remote

const TwinAppID = "dev.wendy.digital-twin"

func (b *Backend) OpenSiteRobot(session uint64, serial string) (*SiteRobot, error) {
	endpoint, err := b.OpenApp(TwinAppID, session)
	if err != nil {
		return nil, err
	}
	r := &twinrobot.Remote{URL: endpoint, Client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}}
	reply, err := r.Call(twinrobot.Request{Operation: "open", Serial: serial})
	if err != nil {
		return nil, err
	}
	r.Handle = reply.Handle
	return r, nil
}
