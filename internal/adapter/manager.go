package adapter

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

type Manager struct {
	lifeMu      sync.Mutex
	mu          sync.RWMutex
	active      sync.WaitGroup
	cfg         Config
	server      *http.Server
	listener    net.Listener
	lastError   string
	generation  uint64
	connections map[*websocket.Conn]struct{}
}

type Status struct {
	Config        Config `json:"config"`
	Running       bool   `json:"running"`
	Error         string `json:"error,omitempty"`
	ListenMode    string `json:"listen_mode,omitempty"`
	ListenAddress string `json:"listen_address,omitempty"`
}

func NewManager() *Manager {
	return &Manager{cfg: DefaultConfig(), connections: make(map[*websocket.Conn]struct{})}
}

func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	status := Status{Config: m.cfg, Running: m.server != nil, Error: m.lastError}
	if m.server != nil {
		status.ListenMode = m.cfg.ListenMode
		status.ListenAddress = m.server.Addr
	}
	return status
}

// Keep legacy configurations editable through management without exposing a listener.
func (m *Manager) configureBlocked(cfg Config, err error) bool {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.server != nil {
		return false
	}
	m.cfg = cfg
	m.lastError = err.Error()
	return true
}

func (m *Manager) Configure(cfg Config) error {
	if cfg.ListenMode == "" {
		cfg.ListenMode = "unix"
	}
	if cfg.ListenAddress == "" && cfg.ListenMode == "unix" {
		cfg.ListenAddress = defaultListenAddress
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	cfg.APIs = slices.Clone(cfg.APIs)
	for i := range cfg.APIs {
		cfg.APIs[i].Mappings = slices.Clone(cfg.APIs[i].Mappings)
		for j := range cfg.APIs[i].Mappings {
			model := &cfg.APIs[i].Mappings[j]
			model.InputModalities = slices.Clone(model.InputModalities)
			model.ReasoningLevels = slices.Clone(model.ReasoningLevels)
		}
	}
	network, address := cfg.listenEndpoint()
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	m.mu.Lock()
	if !reflect.DeepEqual(cfg, m.cfg) {
		m.generation++
		modelCatalogCache.clear()
		for conn := range m.connections {
			_ = conn.Close()
			delete(m.connections, conn)
		}
	}
	oldNetwork, oldAddress := m.cfg.listenEndpoint()
	if m.server != nil && network == oldNetwork && address == oldAddress {
		if network == "unix" && cfg.SocketMode != m.cfg.SocketMode {
			if err := setSocketMode(cfg.SocketPath, cfg.SocketMode); err != nil {
				old, oldListener := m.server, m.listener
				m.server, m.listener = nil, nil
				m.cfg = cfg
				m.lastError = "无法设置套接字权限：" + err.Error()
				m.mu.Unlock()
				closeServer(old, oldListener)
				return nil
			}
		}
		m.cfg = cfg
		m.lastError = ""
		m.mu.Unlock()
		return nil
	}
	old, oldListener := m.server, m.listener
	listener, err := net.Listen(network, address)
	if network == "unix" {
		if err != nil && removeStaleSocket(address) {
			listener, err = net.Listen(network, address)
		}
		if err == nil {
			if err = setSocketMode(address, cfg.SocketMode); err != nil {
				_ = listener.Close()
			}
		}
	}
	m.cfg = cfg
	if err != nil {
		m.server, m.listener = nil, nil
		m.lastError = fmt.Sprintf("无法监听 %s %s：%v", network, address, err)
		m.mu.Unlock()
		if old != nil {
			closeServer(old, oldListener)
		}
		return nil
	}
	server := &http.Server{
		Addr:              listener.Addr().String(),
		Handler:           m,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       requestReadTimeout,
		IdleTimeout:       90 * time.Second,
	}
	m.server = server
	m.listener = listener
	m.lastError = ""
	m.mu.Unlock()
	if old != nil {
		closeServer(old, oldListener)
	}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			m.mu.Lock()
			if m.server == server {
				m.server, m.listener = nil, nil
				m.lastError = "监听服务已停止：" + err.Error()
			}
			m.mu.Unlock()
		}
	}()
	return nil
}

func removeStaleSocket(socketPath string) bool {
	before, err := os.Lstat(socketPath)
	if err != nil || before.Mode()&os.ModeSocket == 0 {
		return false
	}
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err == nil {
		_ = conn.Close()
		return false
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) || (errno != syscall.ECONNREFUSED && !(runtime.GOOS == "windows" && errno == 10061)) {
		return false
	}
	after, err := os.Lstat(socketPath)
	if err != nil || !os.SameFile(before, after) || after.Mode()&os.ModeSocket == 0 {
		return false
	}
	return os.Remove(socketPath) == nil
}

func setSocketMode(socketPath, mode string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	permissions := os.FileMode(0660)
	switch mode {
	case "0600":
		permissions = 0600
	case "0666":
		permissions = 0666
	}
	return os.Chmod(socketPath, permissions)
}

func closeServer(server *http.Server, listener net.Listener) {
	if listener != nil {
		_ = listener.Close()
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = server.Close()
	}()
}

func (m *Manager) Stop() {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	m.mu.Lock()
	server, listener := m.server, m.listener
	m.server, m.listener = nil, nil
	m.generation++
	modelCatalogCache.clear()
	for conn := range m.connections {
		_ = conn.Close()
		delete(m.connections, conn)
	}
	m.mu.Unlock()
	if listener != nil {
		_ = listener.Close()
	}
	if server != nil {
		_ = server.Close()
	}
	m.active.Wait()
}

func (m *Manager) begin() (Config, uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.server == nil {
		return Config{}, 0, false
	}
	m.active.Add(1)
	return m.cfg, m.generation, true
}

func (m *Manager) trackPair(generation uint64, client, upstream *websocket.Conn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.server == nil || m.generation != generation {
		return false
	}
	m.connections[client] = struct{}{}
	m.connections[upstream] = struct{}{}
	return true
}

func (m *Manager) untrack(client, upstream *websocket.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.connections, client)
	delete(m.connections, upstream)
}
