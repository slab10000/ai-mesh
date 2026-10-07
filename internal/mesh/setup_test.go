package mesh

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestDiscoveryMergesTailscaleWithSSHAccount(t *testing.T) {
	tail, err := parseTailscale([]byte(`{"Peer":{"lab":{"HostName":"homelab","DNSName":"homelab.example.ts.net.","TailscaleIPs":["100.64.0.10"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"homelab", "homelab.example.ts.net", "HOMELAB.EXAMPLE.TS.NET.", "100.64.0.10"} {
		t.Run(host, func(t *testing.T) {
			ssh := parseSSHConfig([]byte("Host work-server\n HostName " + host + "\n User alice\n IdentityFile ~/.ssh/work\n"))
			list := mergeCandidates(append(tail, ssh...))
			if len(list) != 1 || list[0].Host != "work-server" || list[0].User != "alice" || list[0].Source != "ssh-config+tailscale" {
				t.Fatalf("lost SSH alias or login default: %+v", list)
			}
		})
	}
	ssh := parseSSHConfig([]byte("Host homelab\n User alice\n"))
	list := mergeCandidates(append(tail, ssh...))
	if len(list) != 1 || list[0].Host != "homelab" || list[0].User != "alice" {
		t.Fatalf("short SSH name was not merged: %+v", list)
	}
}

func TestDiscoveryKeepsDistinctConnections(t *testing.T) {
	tail, err := parseTailscale([]byte(`{"Peer":{"lab":{"HostName":"homelab","DNSName":"homelab.example.ts.net.","TailscaleIPs":["100.64.0.10"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, config := range []string{
		"Host homelab\n HostName 192.168.1.2\n User alice\n",
		"Host homelab\n Port 2222\n User alice\n",
	} {
		list := mergeCandidates(append(tail, parseSSHConfig([]byte(config))...))
		if len(list) != 2 {
			t.Fatalf("merged a different address/port: %+v", list)
		}
	}
	ssh := parseSSHConfig([]byte("Host lab-personal\n HostName homelab\n User alice\nHost lab-admin\n HostName homelab\n User admin\n"))
	list := mergeCandidates(append(tail, ssh...))
	if len(list) != 2 || list[0].User != "admin" || list[1].User != "alice" {
		t.Fatalf("lost a separate SSH account: %+v", list)
	}
}

func TestSetupSignInRetry(t *testing.T) {
	var output bytes.Buffer
	reader := bufio.NewReader(strings.NewReader("homelab\n\nyes\nalice\n\n"))
	var calls []EnrollOptions
	peer, err := setupEnroll(reader, &output, Candidate{Host: "homelab", Name: "homelab", Port: 22}, func(options EnrollOptions) (Peer, error) {
		calls = append(calls, options)
		if len(calls) == 1 {
			return Peer{}, &enrollmentSSHError{errors.New("permission denied")}
		}
		return Peer{Name: options.Name, Endpoint: options.Endpoint}, nil
	})
	if err != nil || peer.Endpoint.User != "alice" || len(calls) != 2 || calls[0].Endpoint.User != "homelab" {
		t.Fatalf("account correction failed: %+v %v %+v", peer, err, calls)
	}
	if !calls[1].Mutual || !calls[1].Integrate || !calls[1].Service {
		t.Fatalf("lost enrollment settings: %+v", calls[1])
	}
	for _, text := range []string{"user account", "Password input is hidden", "Connecting as alice@homelab", "Try signing in again"} {
		if !strings.Contains(output.String(), text) {
			t.Fatalf("missing sign-in guidance %q: %s", text, output.String())
		}
	}
}

func TestSetupUsesSSHAccountDefault(t *testing.T) {
	var output bytes.Buffer
	peer, err := setupEnroll(bufio.NewReader(strings.NewReader("\n\n")), &output,
		Candidate{Host: "lab-alias", Name: "homelab", User: "alice", Port: 2200}, func(options EnrollOptions) (Peer, error) {
			return Peer{Name: options.Name, Endpoint: options.Endpoint}, nil
		})
	if err != nil || peer.Endpoint != (Endpoint{"lab-alias", "alice", 2200}) || peer.Name != "homelab" {
		t.Fatalf("did not retain SSH account and port: %+v %v", peer, err)
	}
}

func TestSetupRequiresAccount(t *testing.T) {
	var output bytes.Buffer
	peer, err := setupEnroll(bufio.NewReader(strings.NewReader("\nalice\n\n")), &output,
		Candidate{Host: "homelab", Name: "homelab", Port: 22}, func(options EnrollOptions) (Peer, error) {
			if options.Endpoint.User == "" {
				t.Fatal("tried connecting without an account")
			}
			return Peer{Endpoint: options.Endpoint}, nil
		})
	if err != nil || peer.Endpoint.User != "alice" {
		t.Fatalf("did not reprompt for account: %+v %v", peer, err)
	}
}

func TestSetupDoesNotRetryPartialEnrollment(t *testing.T) {
	var output bytes.Buffer
	want := errors.New("enrolled; remote service installation failed")
	calls := 0
	_, err := setupEnroll(bufio.NewReader(strings.NewReader("alice\n\nyes\n")), &output,
		Candidate{Host: "homelab", Name: "homelab", Port: 22}, func(options EnrollOptions) (Peer, error) {
			calls++
			return Peer{Name: options.Name}, want
		})
	if !errors.Is(err, want) || calls != 1 || strings.Contains(output.String(), "Try signing in again") {
		t.Fatalf("retried after installation began: %d %v %s", calls, err, output.String())
	}
}

func TestEnrollmentMarksConnectionFailuresBeforeInstalling(t *testing.T) {
	s := fixture(t, "laptop", false)
	fakeSSH(t)
	_, err := s.Enroll(EnrollOptions{Endpoint: Endpoint{"offline.test", "alice", 22}, Name: "homelab"})
	var connectionError *enrollmentSSHError
	if !errors.As(err, &connectionError) {
		t.Fatalf("missing retryable connection error: %v", err)
	}
	config, err := s.Config()
	if err != nil || len(config.Peers) != 0 {
		t.Fatalf("changed roster on failed sign-in: %+v %v", config, err)
	}
}
