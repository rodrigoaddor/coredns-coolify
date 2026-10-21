package coolify

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const maxStateSize = 32 << 20

// Persist per-server records, rather than only the merged answers, so partial
// refresh failures and server removal retain their normal semantics after restart.
type persistedState struct {
	Version    int                            `json:"version"`
	Config     string                         `json:"config"`
	Discovered []string                       `json:"discovered"`
	ByServer   map[string]map[string][]string `json:"by_server"`
}

func (c *Coolify) stateKey() string {
	servers := make([][2]string, 0, len(c.servers))
	for _, s := range c.servers {
		servers = append(servers, [2]string{s.uuid, s.override})
	}
	// Only the digest is written; credentials never appear in the cache file.
	data, _ := json.Marshal([]any{c.baseURL, c.token, servers, c.autoServers, c.zones})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// loadState runs during configuration, before the handler can serve queries.
// Validate the entire file before publishing any of its data.
func (c *Coolify) loadState() error {
	if c.persistPath == "" {
		return nil
	}
	f, err := os.Open(c.persistPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxStateSize+1))
	if err != nil {
		return err
	}
	if len(data) > maxStateSize {
		return fmt.Errorf("cache exceeds 32 MiB")
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("invalid cache JSON")
	}
	if state.Version != 1 || state.Config != c.stateKey() {
		return fmt.Errorf("cache version or configuration mismatch")
	}
	discovered := make([]server, 0, len(state.Discovered))
	active := map[string]bool{}
	for _, s := range c.servers {
		active[s.uuid] = true
	}
	seen := map[string]bool{}
	for _, uuid := range state.Discovered {
		if !c.autoServers || !validServerUUID(uuid) || seen[uuid] {
			return fmt.Errorf("invalid cached server list")
		}
		seen[uuid] = true
		active[uuid] = true
		discovered = append(discovered, server{uuid: uuid})
	}
	sort.Slice(discovered, func(i, j int) bool { return discovered[i].uuid < discovered[j].uuid })
	byServer := map[string]records{}
	for uuid, names := range state.ByServer {
		if !active[uuid] {
			return fmt.Errorf("cache contains an inactive server")
		}
		rs := records{}
		for name, values := range names {
			normalized, err := domainName(name, false)
			if err != nil || normalized != name || len(values) == 0 {
				return fmt.Errorf("invalid cached domain")
			}
			for _, value := range values {
				t, err := parseTarget(value)
				if err != nil || t.alias == name {
					return fmt.Errorf("invalid cached target")
				}
				for _, old := range rs[name] {
					if old == t || old.alias != "" || t.alias != "" {
						return fmt.Errorf("conflicting cached targets")
					}
				}
				rs[name] = append(rs[name], t)
			}
		}
		byServer[uuid] = rs
	}
	c.discovered, c.byServer = discovered, byServer
	servers, _ := c.activeServers()
	c.rebuildSnapshot(servers)
	return nil
}

// saveState is called under refreshMu. Rename a synced temporary file in the
// same directory so interrupted writes cannot truncate the previous cache.
func (c *Coolify) saveState() error {
	if c.persistPath == "" {
		return nil
	}
	state := persistedState{Version: 1, Config: c.stateKey(), ByServer: map[string]map[string][]string{}}
	for _, s := range c.discovered {
		state.Discovered = append(state.Discovered, s.uuid)
	}
	for uuid, rs := range c.byServer {
		names := map[string][]string{}
		for name, ts := range rs {
			for _, t := range ts {
				value := t.alias
				if t.ip.IsValid() {
					value = t.ip.String()
				}
				names[name] = append(names[name], value)
			}
		}
		state.ByServer[uuid] = names
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(data) > maxStateSize {
		return fmt.Errorf("cache exceeds 32 MiB")
	}
	f, err := os.CreateTemp(filepath.Dir(c.persistPath), ".coolify-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), c.persistPath)
}
