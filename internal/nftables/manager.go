package nftables

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/kof8855/ip-geo-exporter/internal/config"
)

const tableName = "ip_geo_exporter"
const tableFamily = "inet"

// FlowElement represents a single element read from an nftables set.
type FlowElement struct {
	IP        string // IP address (v4 or v6)
	Direction string // "upload" or "download"
	Protocol  string // "tcp", "udp", "icmp", "other", "all"
	Bytes     uint64
	Packets   uint64
}

// Manager handles nftables setup, polling, and teardown.
type Manager struct {
	cfg     *config.Config
	sets    []setDef // all created sets
}

type setDef struct {
	name     string
	direction string // "in" or "out"
	protocol string // "tcp", "udp", "icmp", "other", "all"
	ipVer    string // "v4" or "v6"
}

// New creates a new nftables manager.
func New(cfg *config.Config) *Manager {
	return &Manager{cfg: cfg}
}

// Setup creates the nftables table, sets, chains, and rules.
func (m *Manager) Setup() error {
	// First, clean up any existing table with same name
	_ = m.runNft("delete table", "delete table inet %s", tableName)

	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("add table inet %s\n\n", tableName))

	protos := m.cfg.ProtocolsEnabled()
	isAll := protos[0] == "all"
	specificProtos := make([]string, 0)
	if !isAll {
		// Add "other" as the catch-all
		for _, p := range protos {
			specificProtos = append(specificProtos, p)
		}
		specificProtos = append(specificProtos, "other")
	}

	ipVersions := []string{"v4"}
	if m.cfg.EnableIPv6 {
		ipVersions = append(ipVersions, "v6")
	}

	typeStr := map[string]string{"v4": "ipv4_addr", "v6": "ipv6_addr"}
	saddrExpr := map[string]string{"v4": "ip saddr", "v6": "ip6 saddr"}
	daddrExpr := map[string]string{"v4": "ip daddr", "v6": "ip6 daddr"}
	icmpProto := map[string]string{"v4": "icmp", "v6": "icmpv6"}

	// Build set definitions and create sets
	directions := []struct {
		name string
		dir  string // "in" or "out"
	}{
		{"in", "in"},
		{"out", "out"},
	}

	for _, ipVer := range ipVersions {
		for _, dir := range directions {
			if isAll {
				setName := m.cfg.SetName(dir.dir, "all", ipVer)
				m.sets = append(m.sets, setDef{name: setName, direction: dir.dir, protocol: "all", ipVer: ipVer})
				fmt.Fprintf(&buf, "add set inet %s %s {\n", tableName, setName)
				fmt.Fprintf(&buf, "    type %s\n", typeStr[ipVer])
				fmt.Fprintf(&buf, "    flags dynamic,timeout\n")
				fmt.Fprintf(&buf, "    timeout %s\n", m.cfg.SetTimeout)
				fmt.Fprintf(&buf, "    size %d\n", m.cfg.MaxSetEntries)
				buf.WriteString("    counter\n")
				buf.WriteString("}\n\n")
			} else {
				for _, proto := range specificProtos {
					setName := m.cfg.SetName(dir.dir, proto, ipVer)
					m.sets = append(m.sets, setDef{name: setName, direction: dir.dir, protocol: proto, ipVer: ipVer})
					fmt.Fprintf(&buf, "add set inet %s %s {\n", tableName, setName)
					fmt.Fprintf(&buf, "    type %s\n", typeStr[ipVer])
					fmt.Fprintf(&buf, "    flags dynamic,timeout\n")
					fmt.Fprintf(&buf, "    timeout %s\n", m.cfg.SetTimeout)
					fmt.Fprintf(&buf, "    size %d\n", m.cfg.MaxSetEntries)
					buf.WriteString("    counter\n")
					buf.WriteString("}\n\n")
				}
			}
		}
	}

	// Create chains and rules
	for _, ipVer := range ipVersions {
		for _, dir := range directions {
			hook := "input"
			saddr := saddrExpr[ipVer]
			if dir.dir == "out" {
				hook = "output"
				saddr = daddrExpr[ipVer]
			}

			chainName := fmt.Sprintf("%s_%s", dir.dir, ipVer)
			fmt.Fprintf(&buf, "add chain inet %s %s { type filter hook %s priority -100; policy accept; }\n", tableName, chainName, hook)

			if isAll {
				setName := m.cfg.SetName(dir.dir, "all", ipVer)
				fmt.Fprintf(&buf, "add rule inet %s %s update @%s { %s } accept\n", tableName, chainName, setName, saddr)
			} else {
				for _, proto := range protos {
					setName := m.cfg.SetName(dir.dir, proto, ipVer)
					l4proto := proto
					if proto == "icmp" {
						l4proto = icmpProto[ipVer]
					}
					fmt.Fprintf(&buf, "add rule inet %s %s meta l4proto %s update @%s { %s } accept\n",
						tableName, chainName, l4proto, setName, saddr)
				}
				// "other" catch-all (no l4proto match)
				setName := m.cfg.SetName(dir.dir, "other", ipVer)
				fmt.Fprintf(&buf, "add rule inet %s %s update @%s { %s } accept\n", tableName, chainName, setName, saddr)
			}
			buf.WriteString("\n")
		}
	}

	fmt.Printf("[nftables] creating ruleset:\n%s\n", buf.String())
	return m.runNftStdin("setup", buf.String())
}

// Poll reads all sets and returns the current flow elements.
func (m *Manager) Poll() ([]FlowElement, error) {
	var all []FlowElement

	for _, s := range m.sets {
		elements, err := m.readSet(s)
		if err != nil {
			return nil, fmt.Errorf("read set %s: %w", s.name, err)
		}

		dirLabel := "download"
		if s.direction == "out" {
			dirLabel = "upload"
		}

		for _, elem := range elements {
			all = append(all, FlowElement{
				IP:        elem.IP,
				Direction: dirLabel,
				Protocol:  s.protocol,
				Bytes:     elem.Bytes,
				Packets:   elem.Packets,
			})
		}
	}

	return all, nil
}

// Teardown removes the nftables table.
func (m *Manager) Teardown() error {
	return m.runNft("teardown", "delete table inet %s", tableName)
}

type nftElement struct {
	IP      string
	Bytes   uint64
	Packets uint64
}

// readSet reads all elements from a single nftables set.
func (m *Manager) readSet(s setDef) ([]nftElement, error) {
	out, err := exec.Command("nft", "--json", "list", "set", tableFamily, tableName, s.name).Output()
	if err != nil {
		return nil, fmt.Errorf("nft list set %s: %w (output: %s)", s.name, err, string(out))
	}

	return parseNftSetJSON(out)
}

// parseNftSetJSON parses the JSON output of `nft --json list set`.
func parseNftSetJSON(data []byte) ([]nftElement, error) {
	var resp struct {
		Nftables []json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse nftables json: %w", err)
	}

	for _, raw := range resp.Nftables {
		var header struct {
			Set *json.RawMessage `json:"set"`
		}
		if err := json.Unmarshal(raw, &header); err != nil || header.Set == nil {
			continue
		}

		var setObj struct {
			Elem []json.RawMessage `json:"elem"`
		}
		if err := json.Unmarshal(*header.Set, &setObj); err != nil {
			continue
		}

		var result []nftElement
		for _, elemRaw := range setObj.Elem {
			elem, err := parseNftElement(elemRaw)
			if err != nil {
				continue // skip malformed elements
			}
			result = append(result, elem)
		}
		return result, nil
	}

	return nil, nil
}

// parseNftElement extracts IP and counter from a set element.
// The element JSON can be in multiple formats depending on nftables version.
func parseNftElement(raw json.RawMessage) (nftElement, error) {
	// Format 1: {"elem": {"val": "1.2.3.4", "counter": {"packets": 100, "bytes": 5000}}}
	var wrapped struct {
		Elem *struct {
			Val     string `json:"val"`
			Counter *struct {
				Packets uint64 `json:"packets"`
				Bytes   uint64 `json:"bytes"`
			} `json:"counter"`
		} `json:"elem"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Elem != nil {
		elem := nftElement{IP: wrapped.Elem.Val}
		if wrapped.Elem.Counter != nil {
			elem.Packets = wrapped.Elem.Counter.Packets
			elem.Bytes = wrapped.Elem.Counter.Bytes
		}
		return elem, nil
	}

	// Format 2: ["1.2.3.4", {"counter": {"packets": 100, "bytes": 5000}}]
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) >= 2 {
		var ip string
		if err := json.Unmarshal(arr[0], &ip); err == nil {
			var meta struct {
				Counter *struct {
					Packets uint64 `json:"packets"`
					Bytes   uint64 `json:"bytes"`
				} `json:"counter"`
			}
			if err := json.Unmarshal(arr[1], &meta); err == nil {
				elem := nftElement{IP: ip}
				if meta.Counter != nil {
					elem.Packets = meta.Counter.Packets
					elem.Bytes = meta.Counter.Bytes
				}
				return elem, nil
			}
		}
	}

	// Format 3: simple string (just IP, no counter - fallback)
	var ipStr string
	if err := json.Unmarshal(raw, &ipStr); err == nil {
		return nftElement{IP: ipStr}, nil
	}

	return nftElement{}, fmt.Errorf("unrecognized element format: %s", string(raw))
}

func (m *Manager) runNft(desc string, format string, args ...interface{}) error {
	cmd := fmt.Sprintf(format, args...)
	out, err := exec.Command("nft", strings.Fields(cmd)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft %s failed: %w\noutput: %s", desc, err, string(out))
	}
	return nil
}

func (m *Manager) runNftStdin(desc string, ruleset string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(ruleset)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft %s failed: %w\nruleset:\n%s\noutput: %s", desc, err, ruleset, string(out))
	}
	return nil
}

// MustRunNftCheck verifies nftables is available on the system.
func MustRunNftCheck() {
	if err := exec.Command("nft", "--version").Run(); err != nil {
		fmt.Printf("[FATAL] nftables command not found: %v\n", err)
		return
	}
	fmt.Printf("[info] nftables available\n")
}

// init() to set time format for nftables
var _ = func() struct{} {
	// ensure nftables uses local time
	time.Local = time.UTC
	return struct{}{}
}()
