package node

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/zonepub"
	"net/http"
	"time"
)

func (n *Node) zones(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	if _, ok := n.authorize(w, r, "read", nil); !ok {
		return
	}
	n.lock()
	defer n.mu.Unlock()
	rows, err := n.store.Records()
	if err != nil {
		http.Error(w, "zone replica unavailable", 503)
		return
	}
	reply := zonepub.Reply{Records: []authority.SignedRecord{}}
	for _, c := range rows {
		if c.Key.Type != zonepub.Type {
			continue
		}
		digest, err := recordDigest(c)
		if err != nil {
			http.Error(w, "zone proof unavailable", 503)
			return
		}
		proof, ok := n.proofs[digest]
		if !ok {
			http.Error(w, "zone proof unavailable", 503)
			return
		}
		proof.Component = c
		if _, err = zonepub.Verify(proof, n.verifier, time.Now()); err == nil {
			reply.Records = append(reply.Records, proof)
		}
	}
	raw, err := json.Marshal(reply)
	if err != nil || len(raw)+1 > zonepub.MaxReplyBytes {
		http.Error(w, "zone list exceeds response budget", http.StatusRequestEntityTooLarge)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(append(raw, '\n'))
}
