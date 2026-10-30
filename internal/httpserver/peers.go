package httpserver

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// peerProbeTimeout bounds each sibling health probe so the menu stays fast.
const peerProbeTimeout = 700 * time.Millisecond

type peerProject struct {
	Name    string `json:"name"`
	Port    int    `json:"port"`
	Current bool   `json:"current"`
	Online  bool   `json:"online"`
}

// listenerPort is the port of the listener that accepted r, not the Host
// header (which may name a reverse proxy).
func (s *Server) listenerPort(r *http.Request) string {
	address := s.currentConfig().Bind
	if local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		address = local.String()
	}
	_, port, _ := net.SplitHostPort(address)
	return port
}

// peers lists the sibling servers named by ZENNOTES_PEERS_FILE. Paths and
// credentials never leave the server; clients link by port on their own host.
func (s *Server) peers(w http.ResponseWriter, r *http.Request) {
	projects := []peerProject{}
	path := s.currentConfig().PeersFile
	if path == "" {
		writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
		return
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
		return
	}
	var file struct {
		Projects []struct {
			Name string `json:"name"`
			Port int    `json:"port"`
		} `json:"projects"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &file)
	}
	if err != nil {
		http.Error(w, "invalid peers file", http.StatusInternalServerError)
		return
	}
	own, _ := strconv.Atoi(s.listenerPort(r))
	for _, entry := range file.Projects {
		name := strings.TrimSpace(entry.Name)
		if name == "" || entry.Port < 1 || entry.Port > 65535 {
			continue
		}
		projects = append(projects, peerProject{Name: name, Port: entry.Port, Current: entry.Port == own})
	}
	client := &http.Client{Timeout: peerProbeTimeout, Transport: &http.Transport{Proxy: nil}}
	var wg sync.WaitGroup
	for i := range projects {
		if projects[i].Current {
			projects[i].Online = true
			continue
		}
		wg.Add(1)
		go func(p *peerProject) {
			defer wg.Done()
			resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(p.Port) + "/api/healthz")
			if err == nil {
				p.Online = resp.StatusCode == http.StatusOK
				resp.Body.Close()
			}
		}(&projects[i])
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}
