package mesh

import (
	"fmt"
	"os"
	"strings"
)

type Pending struct {
	Peer      Peer     `json:"peer"`
	Action    string   `json:"action"`
	RevokeID  string   `json:"revoke_id,omitempty"`
	CreatedAt string   `json:"created_at"`
	RemoveIDs []string `json:"remove_ids,omitempty"`
}

func (s *Store) queueChange(p Peer, action, id string) error {
	if !validID.MatchString(p.ID) || (id != "" && !validID.MatchString(id)) {
		return fmt.Errorf("invalid pending identity")
	}
	return writeJSON(s.path("pending", action+"-"+p.ID+"-"+id+".json"), Pending{Peer: p, Action: action, RevokeID: id, CreatedAt: now()})
}

// Only user-initiated membership operations are retried. Inventory exchange
// never enrolls a peer or grants an SSH key.
func (s *Store) PendingChanges() []string {
	entries, e := os.ReadDir(s.path("pending"))
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return []string{e.Error()}
	}
	var issues []string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		file := s.path("pending", entry.Name())
		var op Pending
		if e := readJSON(file, &op); e != nil {
			issues = append(issues, e.Error())
			continue
		}
		c, e := s.Config()
		if e != nil {
			return append(issues, e.Error())
		}
		request := Request{Action: op.Action, ID: op.RevokeID}
		if op.Action == "detach" {
			request.RemoveIDs = op.RemoveIDs
		}
		if op.Action == "roster" || op.Action == "peer" {
			if _, ok := c.Peers[op.Peer.ID]; !ok {
				_ = os.Remove(file)
				continue
			}
			request.Peers = []Peer{c.Self}
			if op.Action == "roster" {
				if _, ok := c.Peers[op.RevokeID]; ok {
					request.RejoinIDs = []string{op.RevokeID}
				}
				for _, p := range c.Peers {
					request.Peers = append(request.Peers, p)
				}
			}
		}
		if e := s.Call(op.Peer, request, nil); e != nil {
			issues = append(issues, op.Peer.Name+": "+e.Error())
			continue
		}
		if e := os.Remove(file); e != nil {
			issues = append(issues, e.Error())
		}
	}
	return issues
}

func (s *Store) RemoveFromMesh(name string) error {
	c, e := s.Config()
	if e != nil {
		return e
	}
	p, local, e := c.Resolve(name)
	if e != nil {
		return e
	}
	if local {
		return fmt.Errorf("cannot remove yourself")
	}
	for _, peer := range c.Peers {
		if !peer.Incoming {
			continue
		}
		id := p.ID
		if peer.ID == p.ID {
			ids := []string{c.Self.ID}
			for _, other := range c.Peers {
				if other.ID != p.ID {
					ids = append(ids, other.ID)
				}
			}
			op := Pending{Peer: peer, Action: "detach", CreatedAt: now(), RemoveIDs: ids}
			if e := writeJSON(s.path("pending", "detach-"+peer.ID+".json"), op); e != nil {
				return e
			}
			continue
		}
		if e := s.queueChange(peer, "revoke", id); e != nil {
			return e
		}
	}
	if e := s.RemovePeer(p.ID); e != nil {
		return e
	}
	issues := s.PendingChanges()
	if len(issues) > 0 {
		return fmt.Errorf("removed locally; revocations queued until peers reconnect: %s", strings.Join(issues, "; "))
	}
	return nil
}
