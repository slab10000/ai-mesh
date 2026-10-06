package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Candidate struct {
	Name         string `json:"name"`
	Host         string `json:"host"`
	User         string `json:"user,omitempty"`
	Port         int    `json:"port"`
	Source       string `json:"source"`
	SSHReachable *bool  `json:"ssh_reachable,omitempty"`
}
type tailscaleStatus struct {
	Self *tailscaleNode           `json:"Self"`
	Peer map[string]tailscaleNode `json:"Peer"`
}
type tailscaleNode struct {
	HostName     string   `json:"HostName"`
	DNSName      string   `json:"DNSName"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	Online       bool     `json:"Online"`
	Tags         []string `json:"Tags"`
}

func parseTailscale(b []byte) ([]Candidate, error) {
	var data tailscaleStatus
	if e := json.Unmarshal(b, &data); e != nil {
		return nil, e
	}
	var out []Candidate
	for _, p := range data.Peer {
		infrastructure := false
		for _, tag := range p.Tags {
			if tag == "tag:ingress" {
				infrastructure = true
			}
		}
		if infrastructure {
			continue
		}
		host := strings.TrimSuffix(p.DNSName, ".")
		if host == "" && len(p.TailscaleIPs) > 0 {
			host = p.TailscaleIPs[0]
		}
		if host != "" {
			out = append(out, Candidate{Name: p.HostName, Host: host, Port: 22, Source: "tailscale"})
		}
	}
	return out, nil
}

func parseSSHConfig(b []byte) []Candidate {
	var out []Candidate
	var current []Candidate
	flush := func() { out = append(out, current...); current = nil }
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		fields := strings.Fields(strings.Replace(line, "=", " ", 1))
		if len(fields) < 2 {
			continue
		}
		key := strings.ToLower(fields[0])
		value := strings.Trim(fields[1], "\"'")
		if key == "host" {
			flush()
			for _, host := range fields[1:] {
				if strings.ContainsAny(host, "*?!") {
					continue
				}
				current = append(current, Candidate{Name: host, Host: host, Port: 22, Source: "ssh-config"})
			}
		} else if key == "match" {
			flush()
		} else {
			for i := range current {
				switch key {
				case "user":
					current[i].User = value
				case "port":
					if n, e := strconv.Atoi(value); e == nil {
						current[i].Port = n
					}
				}
			}
		}
	}
	flush()
	return out
}

func (s *Store) Discover(lan, check bool, cidr string) ([]Candidate, []string) {
	var list []Candidate
	var warnings []string
	if _, e := exec.LookPath("tailscale"); e == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		b, e := exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
		cancel()
		if e != nil {
			warnings = append(warnings, "Tailscale discovery unavailable: "+e.Error())
		} else if peers, e := parseTailscale(b); e == nil {
			list = append(list, peers...)
		} else {
			warnings = append(warnings, "Cannot parse Tailscale status: "+e.Error())
		}
	}
	if b, e := os.ReadFile(filepath.Join(s.UserHome, ".ssh", "config")); e == nil {
		list = append(list, parseSSHConfig(b)...)
	}
	if lan {
		b := probe("arp", "-an")
		if b == "" {
			if data, e := os.ReadFile("/proc/net/arp"); e == nil {
				b = string(data)
			}
		}
		for _, ip := range regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`).FindAllString(b, -1) {
			parsed := net.ParseIP(ip)
			if parsed != nil && parsed.IsPrivate() {
				list = append(list, Candidate{Name: ip, Host: ip, Port: 22, Source: "LAN-neighbor"})
			}
		}
	}
	if cidr != "" {
		ip, n, e := net.ParseCIDR(cidr)
		if e != nil {
			warnings = append(warnings, "invalid CIDR")
		} else {
			ones, bits := n.Mask.Size()
			if bits != 32 || ones < 24 {
				warnings = append(warnings, "active discovery is limited to IPv4 /24 or smaller networks")
			} else {
				ip = ip.Mask(n.Mask).To4()
				for x := append(net.IP(nil), ip...); n.Contains(x); incrementIP(x) {
					list = append(list, Candidate{Name: x.String(), Host: x.String(), Port: 22, Source: "LAN-probe"})
				}
				check = true
			}
		}
	}
	unique := map[string]Candidate{}
	for _, p := range list {
		key := p.Host + ":" + strconv.Itoa(p.Port)
		if _, ok := unique[key]; !ok {
			unique[key] = p
		}
	}
	list = nil
	for _, p := range unique {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	if check {
		var wg sync.WaitGroup
		slots := make(chan struct{}, 16)
		for i := range list {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				slots <- struct{}{}
				defer func() { <-slots }()
				host := list[i].Host
				if list[i].Source == "ssh-config" {
					out := probe("ssh", "-G", host)
					for _, line := range strings.Split(out, "\n") {
						if strings.HasPrefix(line, "hostname ") {
							host = strings.TrimPrefix(line, "hostname ")
						}
					}
				}
				conn, e := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(list[i].Port)), 600*time.Millisecond)
				ok := e == nil
				if ok {
					_ = conn.Close()
				}
				list[i].SSHReachable = &ok
			}(i)
		}
		wg.Wait()
	}
	if cidr != "" {
		var filtered []Candidate
		for _, p := range list {
			if p.Source != "LAN-probe" || (p.SSHReachable != nil && *p.SSHReachable) {
				filtered = append(filtered, p)
			}
		}
		list = filtered
	}
	return list, warnings
}
func incrementIP(ip net.IP) {
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			break
		}
	}
}

func suggestedAddress() string {
	if ip := probe("tailscale", "ip", "-4"); net.ParseIP(ip) != nil {
		return ip
	}
	host, _ := os.Hostname()
	return host
}

func printCandidates(list []Candidate) {
	for i, p := range list {
		state := "unchecked"
		if p.SSHReachable != nil {
			state = "unreachable"
			if *p.SSHReachable {
				state = "SSH port reachable"
			}
		}
		fmt.Printf("%2d  %-24s %-32s %s (%s)\n", i+1, p.Name, p.Host, p.Source, state)
	}
}
