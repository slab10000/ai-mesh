package mesh

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const help = `ai-mesh — agents across your computers

Setup
  mesh setup                                Interactive enrollment wizard
  mesh init [--name NAME] [--incoming]        Initialize this account only
  mesh discover [--lan] [--probe] [--json]    Tailscale, SSH config, LAN neighbors
  mesh discover --cidr 192.168.1.0/24         Explicit bounded LAN port scan
  mesh enroll HOST --user USER --name NAME [--port 22] [--mutual]
               [--binary FILE] [--integrate=true] [--service=true]
  mesh peers reconcile                      Grant mutual access to enrolled peers
  mesh peers remove NAME                    Revoke locally and on reachable peers
  mesh access --incoming=true|false         Change this account's incoming access
  mesh ssh-server install --address IP [--port 2222]  Optional macOS user SSH listener
  mesh ssh-server status|uninstall          Inspect/remove that listener
  mesh integrate [--remove]                 Managed instructions for installed agents
  mesh service install|status|uninstall      User launchd/systemd service
  mesh doctor                               Local prerequisite checks

Inventory
  mesh machines [--check] [--json]           Cached inventory; optional live checks
  mesh machine show NAME                    Full cached machine description
  mesh machine refresh                      Refresh this machine's basic facts
  mesh capability add NAME [--environment ENV] [--note TEXT]
  mesh capability remove NAME [--environment ENV]
  mesh sync                                 Publish pending description updates

Tasks
  mesh run --on NAME --agent codex|claude --input PATH --context FILE
           --output DIR [--expect FILE] [--only] [--max-depth 2] [--max-children 4] [--wait] "PROMPT"
  mesh run --on NAME --agent shell [--input PATH] [--wait] -- COMMAND ARG...
  mesh jobs                                 Local jobs and outgoing receipts
  mesh status ID                            Execution and delivery status
  mesh watch ID                             Follow raw agent/command output
  mesh collect ID [--output DIR]            Verify and retrieve deliverables
  mesh retry ID                             Retry the same submission (idempotent)
  mesh cancel ID                            Cancel the named job, not its children
  mesh daemon [--once] [--interval 30s]      Retry pending updates and collect results

Interactive sessions (tmux required on each participating machine)
  mesh codex|claude|gemini|opencode|shell [MACHINE] [--project DIR] [--resume]
  mesh connect                              Choose a machine and an installed agent
  mesh switch MACHINE [--agent AGENT] [--project DIR]
  mesh back                                 Return to the preserved previous session
  mesh sessions                             List saved session groups
  Ctrl-b m / Ctrl-b b                        Computer menu / return (inside Mesh)

Flags precede task prompts/commands. Inputs and outputs are bounded to 32 MiB per
bundle. Agent credentials stay on their host. MESH_HOME selects an isolated state
directory; MESH_USER_HOME overrides user files for testing. No network discovery
or agent configuration changes happen merely by running help or building Mesh.
`

type stringsFlag []string

func (s *stringsFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringsFlag) Set(v string) error { *s = append(*s, v); return nil }
func flags(name string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(os.Stderr)
	return f
}
func printJSON(v any) error {
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	return e.Encode(v)
}
func need(args []string, n int, usage string) error {
	if len(args) < n {
		return errors.New(usage)
	}
	return nil
}

func Main(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(help)
		return nil
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Println("ai-mesh", Version)
		return nil
	}
	s, e := OpenStore()
	if e != nil {
		return e
	}
	extendToolPath(s.UserHome)
	command := args[0]
	args = args[1:]
	switch command {
	case "init":
		f := flags("init")
		name := f.String("name", "", "machine name")
		address := f.String("address", "", "reachable hostname or IP")
		username := f.String("user", "", "SSH account")
		port := f.Int("port", 22, "SSH port")
		incoming := f.Bool("incoming", false, "accept incoming tasks")
		max := f.Int("max-jobs", 2, "local concurrency")
		if e := f.Parse(args); e != nil {
			return e
		}
		p, e := s.Init(*name, *address, *username, *port, *max, *incoming)
		if e != nil {
			return e
		}
		return printJSON(p)
	case "setup":
		return s.Setup()
	case "discover":
		f := flags("discover")
		lan := f.Bool("lan", false, "read LAN neighbor table")
		check := f.Bool("probe", false, "check candidate SSH ports")
		cidr := f.String("cidr", "", "explicit bounded IPv4 network scan")
		jsonOut := f.Bool("json", false, "JSON")
		if e := f.Parse(args); e != nil {
			return e
		}
		list, warnings := s.Discover(*lan, *check, *cidr)
		if *jsonOut {
			return printJSON(map[string]any{"candidates": list, "warnings": warnings})
		}
		for _, w := range warnings {
			fmt.Fprintln(os.Stderr, w)
		}
		printCandidates(list)
		return nil
	case "enroll":
		if e := need(args, 1, "mesh enroll HOST --user USER --name NAME"); e != nil {
			return e
		}
		host := args[0]
		f := flags("enroll")
		user := f.String("user", "", "remote account")
		name := f.String("name", strings.Split(host, ".")[0], "machine name")
		port := f.Int("port", 22, "SSH port")
		binary := f.String("binary", "", "destination binary")
		mutual := f.Bool("mutual", false, "configure mutual access with all enrolled peers")
		integrate := f.Bool("integrate", true, "configure detected agents")
		service := f.Bool("service", true, "install user service")
		if e := f.Parse(args[1:]); e != nil {
			return e
		}
		p, e := s.Enroll(EnrollOptions{Endpoint{host, *user, *port}, *name, *binary, *mutual, *integrate, *service})
		if e != nil {
			return e
		}
		return printJSON(p)
	case "ssh-server":
		if e := need(args, 1, "mesh ssh-server install|status|uninstall"); e != nil {
			return e
		}
		f := flags("ssh-server")
		address := f.String("address", "", "local/private IP to listen on")
		port := f.Int("port", 2222, "unprivileged SSH port")
		if e := f.Parse(args[1:]); e != nil {
			return e
		}
		return s.SSHServer(args[0], *address, *port)
	case "access":
		f := flags("access")
		incoming := f.String("incoming", "", "true or false")
		if e := f.Parse(args); e != nil {
			return e
		}
		value, e := strconv.ParseBool(*incoming)
		if e != nil {
			return errors.New("use mesh access --incoming=true or --incoming=false")
		}
		return s.SetIncoming(value)
	case "peers":
		if e := need(args, 1, "mesh peers reconcile|remove NAME"); e != nil {
			return e
		}
		if args[0] == "reconcile" {
			issues := s.ReconcilePeers()
			if len(issues) > 0 {
				return errors.New(strings.Join(issues, "; "))
			}
			return nil
		}
		if args[0] == "remove" {
			if e := need(args, 2, "mesh peers remove NAME"); e != nil {
				return e
			}
			return s.RemoveFromMesh(args[1])
		}
		return errors.New("unknown peers command")
	case "integrate":
		f := flags("integrate")
		remove := f.Bool("remove", false, "remove managed blocks")
		if e := f.Parse(args); e != nil {
			return e
		}
		paths, e := s.Integrate(*remove)
		for _, p := range paths {
			fmt.Println(p)
		}
		return e
	case "machines":
		f := flags("machines")
		check := f.Bool("check", false, "live checks")
		jsonOut := f.Bool("json", false, "JSON")
		if e := f.Parse(args); e != nil {
			return e
		}
		views, e := s.Machines(*check)
		if e != nil {
			return e
		}
		if *jsonOut {
			return printJSON(views)
		}
		for _, v := range views {
			status := "cached"
			if v.Reachable != nil {
				status = "unreachable"
				if *v.Reachable {
					status = "reachable"
				}
			}
			platform := "unknown"
			if v.Inventory != nil {
				platform = v.Inventory.OS + "/" + v.Inventory.Arch
			}
			fmt.Printf("%-22s %-15s %-12s incoming=%t  %s\n", v.Peer.Name, platform, status, v.Peer.Incoming, v.Peer.ID)
		}
		return nil
	case "machine":
		if e := need(args, 1, "mesh machine show NAME | refresh"); e != nil {
			return e
		}
		if args[0] == "refresh" {
			i, e := s.Refresh()
			if e != nil {
				return e
			}
			for _, issue := range s.Sync() {
				fmt.Fprintln(os.Stderr, "Inventory saved; delivery pending:", issue)
			}
			return printJSON(i)
		}
		if args[0] == "show" {
			if e := need(args, 2, "mesh machine show NAME"); e != nil {
				return e
			}
			c, e := s.Config()
			if e != nil {
				return e
			}
			p, _, e := c.Resolve(args[1])
			if e != nil {
				return e
			}
			var i Inventory
			if e := readJSON(s.path("machines", p.ID+".json"), &i); e != nil {
				return e
			}
			return printJSON(i)
		}
		return errors.New("unknown machine command")
	case "capability":
		if e := need(args, 2, "mesh capability add|remove NAME [--environment ENV] [--note TEXT]"); e != nil {
			return e
		}
		if args[0] != "add" && args[0] != "remove" {
			return errors.New("use capability add or remove")
		}
		f := flags("capability")
		environment := f.String("environment", "", "environment or project scope")
		note := f.String("note", "", "verified observation")
		if e := f.Parse(args[2:]); e != nil {
			return e
		}
		if e := s.Capability(args[1], *environment, *note, args[0] == "remove"); e != nil {
			return e
		}
		for _, issue := range s.Sync() {
			fmt.Fprintln(os.Stderr, "Inventory saved; delivery pending:", issue)
		}
		return nil
	case "sync":
		issues := s.Sync()
		if len(issues) > 0 {
			return errors.New(strings.Join(issues, "; "))
		}
		return nil
	case "run":
		return s.RunCLI(args)
	case "jobs":
		jobs, e := s.LocalJobs()
		if e != nil {
			return e
		}
		receipts := []Receipt{}
		entries, _ := os.ReadDir(s.path("receipts"))
		for _, x := range entries {
			if strings.HasSuffix(x.Name(), ".request.json") {
				continue
			}
			r, e := s.Receipt(strings.TrimSuffix(x.Name(), ".json"))
			if e == nil {
				receipts = append(receipts, r)
			}
		}
		return printJSON(map[string]any{"local": jobs, "outgoing": receipts})
	case "status", "watch", "cancel", "retry", "collect":
		if e := need(args, 1, "mesh "+command+" JOB_ID"); e != nil {
			return e
		}
		id := args[0]
		switch command {
		case "status":
			j, e := s.Status(id)
			if e != nil {
				return e
			}
			return printJSON(j)
		case "watch":
			return s.Wait(id, true)
		case "cancel":
			return s.CancelTask(id)
		case "retry":
			return s.Retry(id)
		case "collect":
			f := flags("collect")
			output := f.String("output", "", "destination")
			if e := f.Parse(args[1:]); e != nil {
				return e
			}
			path, e := s.Collect(id, *output)
			if e != nil {
				return e
			}
			fmt.Println(path)
			return nil
		}
	case "daemon":
		f := flags("daemon")
		once := f.Bool("once", false, "one pass")
		interval := f.Duration("interval", 30*time.Second, "sync interval")
		if e := f.Parse(args); e != nil {
			return e
		}
		return s.Daemon(*interval, *once)
	case "service":
		if e := need(args, 1, "mesh service install|status|uninstall"); e != nil {
			return e
		}
		return s.Service(args[0])
	case "doctor":
		return s.Doctor()
	case "codex", "claude", "gemini", "opencode", "shell":
		target := "local"
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			target = args[0]
			args = args[1:]
		}
		f := flags(command)
		project := f.String("project", "", "working directory on destination")
		resume := f.Bool("resume", false, "reattach saved Mesh session or provider resume picker")
		if e := f.Parse(args); e != nil {
			return e
		}
		return s.StartSession(command, target, *project, *resume)
	case "connect":
		return s.ConnectPicker()
	case "switch":
		if e := need(args, 1, "mesh switch MACHINE [--agent AGENT] [--project DIR]"); e != nil {
			return e
		}
		f := flags("switch")
		agent := f.String("agent", "", "destination agent")
		project := f.String("project", "", "remote project path")
		if e := f.Parse(args[1:]); e != nil {
			return e
		}
		return RequestSwitch(args[0], *agent, *project, false)
	case "back":
		return RequestSwitch("", "", "", true)
	case "sessions":
		entries, e := os.ReadDir(s.path("sessions"))
		if e != nil {
			return e
		}
		var rows []map[string]any
		for _, x := range entries {
			if strings.HasPrefix(x.Name(), "remote-") || !strings.HasSuffix(x.Name(), ".json") {
				continue
			}
			g, e := s.session(strings.TrimSuffix(x.Name(), ".json"))
			if e == nil {
				rows = append(rows, map[string]any{"id": g.ID, "created_at": g.CreatedAt, "windows": g.Windows, "active": s.tmux("has-session", "-t", "mesh-"+g.ID).Run() == nil})
			}
		}
		return printJSON(rows)
	case "_rpc":
		return s.RPC(os.Stdin, os.Stdout)
	case "_identity":
		c, e := s.Config()
		if e != nil {
			return e
		}
		return printJSON(c.Self)
	case "_worker":
		if e := need(args, 1, "missing worker ID"); e != nil {
			return e
		}
		return s.Worker(args[0])
	case "_controller":
		if e := need(args, 1, "missing session ID"); e != nil {
			return e
		}
		return s.Controller(args[0])
	case "_menu", "_select", "_return":
		if e := need(args, 2, "missing session and window"); e != nil {
			return e
		}
		if command == "_menu" {
			return s.ComputerMenu(args[0], args[1])
		}
		if command == "_return" {
			return s.SessionControl(args[0], args[1], "", true)
		}
		if e := need(args, 3, "missing destination"); e != nil {
			return e
		}
		return s.SessionControl(args[0], args[1], args[2], false)
	case "_pane":
		if e := need(args, 2, "missing pane identity"); e != nil {
			return e
		}
		return s.Pane(args[0], args[1])
	case "_remote-session":
		if e := need(args, 1, "missing session ID"); e != nil {
			return e
		}
		return s.RemoteSession(args[0])
	case "_native":
		if e := need(args, 1, "missing session ID"); e != nil {
			return e
		}
		return s.Native(args[0])
	default:
		return fmt.Errorf("unknown command %q; run mesh help", command)
	}
	return nil
}

func (s *Store) RunCLI(args []string) error {
	f := flags("run")
	target := f.String("on", "local", "destination")
	agent := f.String("agent", "codex", "codex, claude, shell")
	output := f.String("output", "", "local output directory")
	contextFile := f.String("context", "", "task brief file")
	only := f.Bool("only", false, "forbid further delegation")
	depth := f.Int("max-depth", 2, "maximum delegation depth")
	children := f.Int("max-children", 4, "children per task")
	wait := f.Bool("wait", false, "follow and collect")
	var inputs stringsFlag
	var expected stringsFlag
	f.Var(&inputs, "input", "input file or directory (repeatable)")
	f.Var(&expected, "expect", "required file inside outputs/ (repeatable)")
	if e := f.Parse(args); e != nil {
		return e
	}
	t := Task{Agent: *agent, MaxDepth: *depth, MaxChildren: *children, RequiredOutputs: expected}
	if *agent == "shell" {
		t.Command = f.Args()
	} else {
		t.Prompt = strings.Join(f.Args(), " ")
	}
	if *contextFile != "" {
		b, e := os.ReadFile(*contextFile)
		if e != nil {
			return e
		}
		if len(b) > 1<<20 {
			return errors.New("context brief exceeds 1 MiB")
		}
		t.Prompt += "\n\nTask context supplied by the originating agent:\n" + string(b)
	}
	if *only {
		c, e := s.Config()
		if e != nil {
			return e
		}
		p, _, e := c.Resolve(*target)
		if e != nil {
			return e
		}
		t.Allowed = []string{p.ID}
		t.MaxDepth = 0
		if parent := os.Getenv("MESH_JOB_ID"); parent != "" {
			j, e := s.Job(parent)
			if e != nil {
				return e
			}
			t.MaxDepth = j.Task.Depth + 1
		}
	}
	var e error
	t.Inputs, e = gatherFiles(inputs)
	if e != nil {
		return e
	}
	r, e := s.Submit(*target, t, *output)
	if e != nil {
		return e
	}
	if e := printJSON(r); e != nil {
		return e
	}
	if *wait {
		if e := s.Wait(r.ID, true); e != nil {
			return e
		}
		path, e := s.Collect(r.ID, "")
		if e != nil {
			return e
		}
		fmt.Println("Results:", path)
	}
	return nil
}

func ask(reader *bufio.Reader, prompt, defaultValue string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	if defaultValue != "" {
		fmt.Fprintf(os.Stderr, " [%s]", defaultValue)
	}
	fmt.Fprint(os.Stderr, ": ")
	line, e := reader.ReadString('\n')
	if e != nil {
		return "", e
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultValue, nil
	}
	return line, nil
}
func yes(reader *bufio.Reader, prompt string) (bool, error) {
	for {
		value, e := ask(reader, prompt+" (yes/no)", "no")
		if e != nil {
			return false, e
		}
		switch strings.ToLower(value) {
		case "yes", "y":
			return true, nil
		case "no", "n":
			return false, nil
		}
	}
}

func (s *Store) Setup() error {
	r := bufio.NewReader(os.Stdin)
	incoming, e := yes(r, "Allow other enrolled computers to run tasks on this computer?")
	if e != nil {
		return e
	}
	mutual, e := yes(r, "Install Mesh and configure mutual access on selected computers?")
	if e != nil {
		return e
	}
	host, _ := os.Hostname()
	name, e := ask(r, "Name this computer", strings.Split(host, ".")[0])
	if e != nil {
		return e
	}
	address, e := ask(r, "Address other computers can use to reach this computer", suggestedAddress())
	if e != nil {
		return e
	}
	if _, e := s.Init(name, address, "", 22, 2, incoming); e != nil {
		return e
	}
	if e := s.SetIncoming(incoming); e != nil {
		return e
	}
	paths, e := s.Integrate(false)
	if e != nil {
		return e
	}
	for _, p := range paths {
		fmt.Println("Configured", p)
	}
	var setupErrors []error
	if e := s.Service("install"); e != nil {
		fmt.Fprintln(os.Stderr, "Service setup:", e)
		setupErrors = append(setupErrors, fmt.Errorf("local service: %w", e))
	}
	if !mutual {
		return errors.Join(setupErrors...)
	}
	candidates, warnings := s.Discover(true, false, "")
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, w)
	}
	printCandidates(candidates)
	selection, e := ask(r, "Select computer numbers separated by commas, or enter a hostname", "")
	if e != nil {
		return e
	}
	if selection == "" {
		return errors.Join(setupErrors...)
	}
	for _, item := range strings.Split(selection, ",") {
		item = strings.TrimSpace(item)
		candidate := Candidate{Host: item, Name: strings.Split(item, ".")[0], Port: 22}
		if n, e := strconv.Atoi(item); e == nil {
			if n < 1 || n > len(candidates) {
				return errors.New("invalid computer number")
			}
			candidate = candidates[n-1]
		}
		username, e := ask(r, "SSH username for "+candidate.Host, candidate.User)
		if e != nil {
			return e
		}
		name, e := ask(r, "Mesh name for "+candidate.Host, candidate.Name)
		if e != nil {
			return e
		}
		fmt.Fprintln(os.Stderr, "SSH will ask for any required password, MFA, and host verification directly.")
		p, e := s.Enroll(EnrollOptions{Endpoint{candidate.Host, username, candidate.Port}, name, "", true, true, true})
		if e != nil {
			fmt.Fprintln(os.Stderr, "Enrollment:", e)
			setupErrors = append(setupErrors, fmt.Errorf("%s: %w", candidate.Host, e))
			continue
		}
		fmt.Println("Enrolled", p.Name)
	}
	return errors.Join(setupErrors...)
}

func (s *Store) ConnectPicker() error {
	views, e := s.Machines(false)
	if e != nil {
		return e
	}
	for i, v := range views {
		fmt.Printf("%d  %s\n", i+1, v.Peer.Name)
	}
	r := bufio.NewReader(os.Stdin)
	choice, e := ask(r, "Computer", "1")
	if e != nil {
		return e
	}
	n, e := strconv.Atoi(choice)
	if e != nil || n < 1 || n > len(views) {
		return errors.New("invalid computer")
	}
	agent, e := ask(r, "Agent (codex, claude, gemini, opencode)", "codex")
	if e != nil {
		return e
	}
	project, e := ask(r, "Project directory on that computer (blank for home)", "")
	if e != nil {
		return e
	}
	return s.StartSession(agent, views[n-1].Peer.ID, project, false)
}

func (s *Store) Doctor() error {
	checks := map[string]any{"version": Version, "state_directory": s.Root}
	for _, tool := range []string{"ssh", "ssh-keygen", "tmux", "codex", "claude", "gemini", "opencode", "tailscale"} {
		path, e := exec.LookPath(tool)
		if e != nil {
			checks[tool] = "not found"
		} else {
			checks[tool] = path
		}
	}
	if c, e := s.Config(); e == nil {
		checks["machine"] = c.Self.Name
		checks["incoming"] = c.Self.Incoming
		checks["peers"] = len(c.Peers)
	} else {
		checks["configuration"] = e.Error()
	}
	if cwd, e := os.Getwd(); e == nil {
		checks["working_directory"] = filepath.Clean(cwd)
	}
	return printJSON(checks)
}
