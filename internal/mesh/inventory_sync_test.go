package mesh

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func inventoryPair(t *testing.T, a, b *Store) {
	t.Helper()
	if err := a.ImportPeers([]Peer{peerOf(b)}); err != nil {
		t.Fatal(err)
	}
	if err := b.ImportPeers([]Peer{peerOf(a)}); err != nil {
		t.Fatal(err)
	}
}

func TestInventoryPublishesOnlyChangesAndRetriesPendingPeers(t *testing.T) {
	a, b, c, d := fixture(t, "a", true), fixture(t, "b", true), fixture(t, "c", true), fixture(t, "d", true)
	inventoryPair(t, a, b)
	inventoryPair(t, a, c)
	fakeSSH(t, a, b, c, d)
	log := filepath.Join(t.TempDir(), "calls.jsonl")
	t.Setenv("MESH_TEST_SSH_LOG", log)
	calls := func() int {
		out, _ := os.ReadFile(log)
		return strings.Count(string(out), "\n")
	}
	syncOK := func() {
		t.Helper()
		if issues := a.Sync(); len(issues) > 0 {
			t.Fatal(issues)
		}
	}
	syncOK()
	if calls() != 2 {
		t.Fatal("initial publication should contact both peers", calls())
	}
	for _, peer := range []*Store{b, c} {
		var got Inventory
		if err := readJSON(peer.path("machines", peerOf(a).ID+".json"), &got); err != nil || got.Revision != 1 {
			t.Fatalf("initial inventory not delivered: %+v %v", got, err)
		}
	}
	// Simulate a restart: acknowledgments must be durable, not held in RAM.
	a = &Store{Root: a.Root, UserHome: a.UserHome}
	for i := 0; i < 3; i++ {
		if issues := a.Tick(); len(issues) > 0 {
			t.Fatal(issues)
		}
	}
	if calls() != 2 {
		t.Fatal("unchanged inventory caused periodic SSH traffic", calls())
	}
	if err := a.Capability("pdf", "python", "verified", false); err != nil {
		t.Fatal(err)
	}
	// The CLI and daemon can observe the same update concurrently.
	concurrent := make(chan []string, 2)
	for i := 0; i < 2; i++ {
		go func() { concurrent <- a.Sync() }()
	}
	for i := 0; i < 2; i++ {
		if issues := <-concurrent; len(issues) > 0 {
			t.Fatal(issues)
		}
	}
	syncOK()
	if calls() != 4 {
		t.Fatal("update should be delivered exactly once per peer", calls())
	}
	// A valid direct edit that leaves the revision untouched still propagates.
	own, _ := a.OwnInventory()
	previous := own.Revision
	own.GPU = "test GPU"
	if err := writeJSON(a.path("machines", own.ID+".json"), own); err != nil {
		t.Fatal(err)
	}
	syncOK()
	var cached Inventory
	if err := readJSON(b.path("machines", own.ID+".json"), &cached); err != nil || cached.GPU != "test GPU" || cached.Revision <= previous {
		t.Fatalf("direct edit did not advance peer copy: %+v %v", cached, err)
	}
	if err := os.WriteFile(c.path("offline"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Capability("pdf", "python", "new observation", false); err != nil {
		t.Fatal(err)
	}
	before := calls()
	if issues := a.Sync(); len(issues) != 1 {
		t.Fatalf("expected one offline peer: %v", issues)
	}
	if calls()-before != 2 {
		t.Fatal("changed description did not target both peers")
	}
	before = calls()
	if issues := a.Sync(); len(issues) != 1 || calls()-before != 1 {
		t.Fatal("retry contacted an already acknowledged peer", issues, calls()-before)
	}
	if err := os.Remove(c.path("offline")); err != nil {
		t.Fatal(err)
	}
	before = calls()
	syncOK()
	syncOK()
	if calls()-before != 1 {
		t.Fatal("reconnected peer was not retried exactly once")
	}
	if err := a.Capability("pdf", "python", "", true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.path("drop-reply"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if issues := a.Sync(); len(issues) != 1 {
		t.Fatal("lost reply not retained for retry", issues)
	}
	before = calls()
	syncOK()
	syncOK()
	if calls()-before != 1 {
		t.Fatal("lost acknowledgment retried the wrong peers")
	}
	cached = Inventory{}
	if err := readJSON(b.path("machines", own.ID+".json"), &cached); err != nil || len(cached.Capabilities) != 0 {
		t.Fatal("capability removal was not delivered", err)
	}
	inventoryPair(t, a, d)
	before = calls()
	syncOK()
	if calls()-before != 1 {
		t.Fatal("new peer should get one initial publication")
	}
	if err := a.RemovePeer(peerOf(b).ID); err != nil {
		t.Fatal(err)
	}
	if err := a.unrevoke(peerOf(b).ID); err != nil {
		t.Fatal(err)
	}
	inventoryPair(t, a, b)
	before = calls()
	syncOK()
	if calls()-before != 1 {
		t.Fatal("re-enrolled peer reused an obsolete acknowledgment")
	}
}

func TestInventoryWatcherIgnoresPeerCopiesAndHandlesAtomicSaves(t *testing.T) {
	a, b := fixture(t, "a", true), fixture(t, "b", true)
	inventoryPair(t, a, b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes, closeWatcher, err := a.inventoryChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWatcher()
	for i := 0; i < 2; i++ {
		if err := a.Capability("test", "", "updated", false); err != nil {
			t.Fatal(err)
		}
		select {
		case <-changes:
		case <-time.After(3 * time.Second):
			t.Fatal("atomic replacement lost the file watch")
		}
	}
	inv, _ := b.OwnInventory()
	if err := a.CacheInventory(inv.ID, inv); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(a.path("daemon-status.json"), map[string]string{"test": "unrelated"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
		t.Fatal("peer cache or unrelated file triggered publication")
	case <-time.After(300 * time.Millisecond):
	}
	if err := a.updateConfig(func(c *Config) error { c.MaxJobs = 3; return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	case <-time.After(3 * time.Second):
		t.Fatal("membership file changes were not observed")
	}
}

func TestDaemonPublishesDirectEditsBeforePeriodicTick(t *testing.T) {
	a, b := fixture(t, "a", true), fixture(t, "b", true)
	inventoryPair(t, a, b)
	fakeSSH(t, a, b)
	cmd := cliCmd(t, a, "daemon", "--interval", "1h")
	log, err := os.Create(a.path("logs", "test-daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("daemon did not shut down cleanly")
		}
	}()
	until := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(a.path("daemon-status.json")); err == nil {
			break
		}
		if time.Now().After(until) {
			out, _ := os.ReadFile(log.Name())
			t.Fatalf("daemon did not initialize: %s", out)
		}
		time.Sleep(30 * time.Millisecond)
	}
	inv, _ := a.OwnInventory()
	inv.CPU = "direct file update from editor"
	// Deliberately do not increment Revision and do not call mesh sync.
	if err := writeJSON(a.path("machines", inv.ID+".json"), inv); err != nil {
		t.Fatal(err)
	}
	until = time.Now().Add(5 * time.Second)
	for {
		var cached Inventory
		if err := readJSON(b.path("machines", inv.ID+".json"), &cached); err == nil && cached.CPU == inv.CPU && cached.Revision > inv.Revision {
			break
		}
		if time.Now().After(until) {
			out, _ := os.ReadFile(log.Name())
			t.Fatalf("edit was not published ahead of 1-hour tick: %s", out)
		}
		time.Sleep(30 * time.Millisecond)
	}
	// The publication request returns an acknowledgment only, not b's specs.
	response, err := b.Handle(Request{Action: "inventory", Inventory: &inv})
	if err != nil || response != nil {
		b, _ := json.Marshal(response)
		t.Fatalf("inventory publication echoed peer specs: %s %v", b, err)
	}
}
