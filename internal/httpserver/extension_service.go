package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

const extensionLogDir = ".maxa/tmp/web-extensions"

type extensionProcess struct {
	cmd    *exec.Cmd
	done   chan struct{}
	target *url.URL
}
type extensionFlight struct {
	done chan struct{}
	err  error
}
type extensionSlot struct {
	mu       sync.Mutex
	process  *extensionProcess
	identity string
	flight   *extensionFlight
	active   int
	idle     *time.Timer
}
type extensionSupervisor struct {
	mu                             sync.Mutex
	slots                          map[string]*extensionSlot
	ctx                            context.Context
	cancel                         context.CancelFunc
	healthWait, idleWait, killWait time.Duration
}

func newExtensionSupervisor() *extensionSupervisor {
	ctx, cancel := context.WithCancel(context.Background())
	return &extensionSupervisor{slots: map[string]*extensionSlot{}, ctx: ctx, cancel: cancel, healthWait: 30 * time.Second, idleWait: 10 * time.Minute, killWait: 5 * time.Second}
}
func processLive(p *extensionProcess) bool {
	if p == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}
func stopExtension(p *extensionProcess, grace time.Duration) {
	if !processLive(p) {
		return
	}
	signalExtension(p.cmd, false)
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-p.done:
		signalExtension(p.cmd, true)
	case <-timer.C:
		signalExtension(p.cmd, true)
		<-p.done
	}
}
func (m *extensionSupervisor) close() {
	m.cancel()
	m.mu.Lock()
	slots := make([]*extensionSlot, 0, len(m.slots))
	for _, s := range m.slots {
		slots = append(slots, s)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range slots {
		wg.Add(1)
		go func(s *extensionSlot) {
			defer wg.Done()
			for {
				s.mu.Lock()
				if s.idle != nil {
					s.idle.Stop()
				}
				if s.flight != nil {
					done := s.flight.done
					s.mu.Unlock()
					<-done
					continue
				}
				stopExtension(s.process, m.killWait)
				s.process = nil
				s.mu.Unlock()
				return
			}
		}(s)
	}
	wg.Wait()
}

// Close stops extension services, including services still waiting for health.
func (s *Server) Close() { s.extensions.close() }
func (m *extensionSupervisor) acquire(ctx context.Context, e extension) (*extensionProcess, func(), error) {
	key := e.project + "\x00" + e.Name
	m.mu.Lock()
	if m.ctx.Err() != nil {
		m.mu.Unlock()
		return nil, nil, m.ctx.Err()
	}
	slot := m.slots[key]
	if slot == nil {
		slot = &extensionSlot{}
		m.slots[key] = slot
	}
	m.mu.Unlock()
	identity := e.root + "\x00" + e.Version
	for {
		slot.mu.Lock()
		if m.ctx.Err() != nil {
			slot.mu.Unlock()
			return nil, nil, m.ctx.Err()
		}
		if slot.flight != nil {
			f := slot.flight
			slot.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-f.done:
				if f.err != nil {
					return nil, nil, f.err
				}
				continue
			}
		}
		if slot.identity == identity && processLive(slot.process) {
			if slot.idle != nil {
				slot.idle.Stop()
			}
			slot.active++
			p := slot.process
			slot.mu.Unlock()
			return p, func() {
				slot.mu.Lock()
				defer slot.mu.Unlock()
				slot.active--
				if slot.active == 0 && slot.process == p {
					slot.idle = time.AfterFunc(m.idleWait, func() {
						slot.mu.Lock()
						defer slot.mu.Unlock()
						if slot.active == 0 && slot.process == p {
							stopExtension(p, m.killWait)
							slot.process = nil
						}
					})
				}
			}, nil
		}
		f := &extensionFlight{done: make(chan struct{})}
		slot.flight = f
		old := slot.process
		slot.process = nil
		if slot.idle != nil {
			slot.idle.Stop()
		}
		slot.mu.Unlock()
		go func() {
			stopExtension(old, m.killWait)
			p, err := m.start(e)
			slot.mu.Lock()
			slot.identity = identity
			slot.process = p
			f.err = err
			slot.flight = nil
			// If the triggering request disconnected, still retire an unused service.
			if p != nil {
				slot.idle = time.AfterFunc(m.idleWait, func() {
					slot.mu.Lock()
					defer slot.mu.Unlock()
					if slot.active == 0 && slot.process == p {
						stopExtension(p, m.killWait)
						slot.process = nil
					}
				})
			}
			close(f.done)
			slot.mu.Unlock()
		}()
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-f.done:
			if f.err != nil {
				return nil, nil, f.err
			}
		}
	}
}
func (m *extensionSupervisor) start(e extension) (*extensionProcess, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	logPath := filepath.Join(e.project, extensionLogDir, e.Name+".log")
	if err = os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	replace := strings.NewReplacer("{root}", e.root, "{project_root}", e.project, "{port}", strconv.Itoa(port))
	argv := make([]string, len(e.manifest.Service.Command))
	for i, arg := range e.manifest.Service.Command {
		argv[i] = replace.Replace(arg)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = e.root
	cmd.Stdout = log
	cmd.Stderr = log
	// exec.Cmd merges duplicate environment keys with the last occurrence winning.
	cmd.Env = append(os.Environ(), "ZENNOTES_EXT_PORT="+strconv.Itoa(port), "MAXA_PROJECT_ROOT="+e.project)
	prepareExtension(cmd)
	if err = cmd.Start(); err != nil {
		fmt.Fprintln(log, "extension start failed:", err)
		log.Close()
		return nil, err
	}
	target, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(port))
	p := &extensionProcess{cmd: cmd, done: make(chan struct{}), target: target}
	go func() {
		err := cmd.Wait()
		signalExtension(cmd, true) // Retire descendants immediately, not on a later PID reuse.
		if err != nil {
			fmt.Fprintln(log, "extension exited:", err)
		}
		log.Close()
		close(p.done)
	}()
	ctx, cancel := context.WithTimeout(m.ctx, m.healthWait)
	defer cancel()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", target.String()+e.manifest.Service.Health, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 && processLive(p) {
				return p, nil
			}
		}
		select {
		case <-p.done:
			stopExtension(p, m.killWait)
			return nil, errors.New("extension exited before health")
		case <-ctx.Done():
			stopExtension(p, m.killWait)
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
func (s *Server) extensionProxy(w http.ResponseWriter, r *http.Request) {
	e, ok := s.selectedExtension(chi.URLParam(r, "name"))
	if !ok || !e.Service {
		http.NotFound(w, r)
		return
	}
	path, err := extensionRoutePath(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p, release, err := s.extensions.acquire(r.Context(), e)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "extension service failed to start", "log": extensionLogDir + "/" + e.Name + ".log"})
		return
	}
	defer release()
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(p.target)
			pr.Out.URL.Path = "/" + path
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = r.URL.RawQuery
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeJSON(w, 502, map[string]string{"error": "extension service unavailable"})
		},
	}
	proxy.ServeHTTP(w, r)
}
