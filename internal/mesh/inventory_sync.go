package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

type inventoryDelivery struct {
	Digest      string `json:"digest"`
	Destination string `json:"destination"`
}

type inventorySyncState struct {
	Revision uint64                       `json:"revision"`
	Content  string                       `json:"content"`
	Peers    map[string]inventoryDelivery `json:"peers"`
}

// Sync publishes only descriptions a destination has not acknowledged. The
// durable per-peer receipts also cover restarts, offline peers, and lost replies.
// It never polls a peer for an unchanged description.
func (s *Store) Sync() []string {
	var issues []string
	err := withLock(s.path("inventory-sync.lock"), func() error {
		c, err := s.Config()
		if err != nil {
			return err
		}
		var state inventorySyncState
		if err := readJSON(s.path("inventory-sync.json"), &state); err != nil && !os.IsNotExist(err) {
			return err
		}
		if state.Peers == nil {
			state.Peers = map[string]inventoryDelivery{}
		}
		before, _ := json.Marshal(state)
		var own Inventory
		// Mesh CLI writers bump the revision themselves. A direct file edit may
		// not, so normalize it before publishing without overwriting a CLI edit.
		err = withLock(s.path("inventory.lock"), func() error {
			var err error
			own, err = s.OwnInventory()
			if err != nil {
				return err
			}
			if own.ID != c.Self.ID || own.Name != c.Self.Name {
				return fmt.Errorf("own inventory identity does not match this computer")
			}
			content := own
			content.Revision, content.UpdatedAt = 0, ""
			b, _ := json.Marshal(content)
			hash := digest(b)
			if own.Revision < state.Revision || (hash != state.Content && own.Revision <= state.Revision) {
				if state.Revision == ^uint64(0) {
					return fmt.Errorf("inventory revision exhausted")
				}
				own.Revision, own.UpdatedAt = state.Revision+1, now()
				if err := writeJSON(s.path("machines", own.ID+".json"), own); err != nil {
					return err
				}
			}
			state.Revision, state.Content = own.Revision, hash
			return nil
		})
		if err != nil {
			return err
		}
		for id := range state.Peers {
			if _, exists := c.Peers[id]; !exists {
				delete(state.Peers, id)
			}
		}
		// Save normalization before attempting network I/O. A crash must not
		// lose the high-water revision needed for subsequent direct file edits.
		baseline, _ := json.Marshal(state)
		if string(baseline) != string(before) {
			if err := writeJSON(s.path("inventory-sync.json"), state); err != nil {
				return err
			}
		}
		payload, _ := json.Marshal(own)
		for _, p := range c.Peers {
			if !p.Incoming {
				continue
			}
			peer, _ := json.Marshal(p)
			wanted := inventoryDelivery{Digest: digest(payload), Destination: digest(peer)}
			if state.Peers[p.ID] == wanted {
				continue
			}
			if err := s.Call(p, Request{Action: "inventory", Inventory: &own}, nil); err != nil {
				issues = append(issues, p.Name+": "+err.Error())
				continue
			}
			state.Peers[p.ID] = wanted
			if err := writeJSON(s.path("inventory-sync.json"), state); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		issues = append(issues, err.Error())
	}
	return issues
}

func (s *Store) forgetInventoryDelivery(id string) error {
	return withLock(s.path("inventory-sync.lock"), func() error {
		var state inventorySyncState
		if err := readJSON(s.path("inventory-sync.json"), &state); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
		if _, exists := state.Peers[id]; !exists {
			return nil
		}
		delete(state.Peers, id)
		return writeJSON(s.path("inventory-sync.json"), state)
	})
}

// Watch parent directories so atomic editor saves remain observable. Peer cache
// writes are excluded, preventing update loops between computers. Events are
// coalesced while an editor writes a file or replaces it by rename.
func (s *Store) inventoryChanges(ctx context.Context) (<-chan struct{}, func(), error) {
	c, err := s.Config()
	if err != nil {
		return nil, nil, err
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, err
	}
	for _, path := range []string{s.Root, s.path("machines")} {
		if err := watcher.Add(path); err != nil {
			watcher.Close()
			return nil, nil, err
		}
	}
	changes := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var timer *time.Timer
		var ready <-chan time.Time
		defer func() {
			if timer != nil {
				timer.Stop()
			}
		}()
		schedule := func() {
			if timer == nil {
				timer = time.NewTimer(100 * time.Millisecond)
			} else {
				timer.Reset(100 * time.Millisecond)
			}
			ready = timer.C
		}
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				path := filepath.Clean(event.Name)
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) != 0 &&
					(path == s.path("machines", c.Self.ID+".json") || path == s.path("config.json")) {
					schedule()
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				fmt.Fprintln(os.Stderr, "inventory file watcher:", err)
				schedule() // Recover dropped events by comparing durable receipts.
			case <-ready:
				ready = nil
				select {
				case changes <- struct{}{}:
				default:
				}
			}
		}
	}()
	closeWatcher := func() { _ = watcher.Close(); <-done }
	return changes, closeWatcher, nil
}
