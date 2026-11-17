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
	lastError   string
	generation  uint64
	connections map[*websocket.Conn]struct{}
}

type Status struct {
	Config  Config `json:"config"`
	Running bool   `json:"running"`
	Error   string `json:"error,omitempty"`
}

func NewManager() *Manager {
	return &Manager{cfg: DefaultConfig(), connections: make(map[*websocket.Conn]struct{})}
}

func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return Status{Config: m.cfg, Running: m.server != nil, Error: m.lastError}
}

func (m *Manager) Configure(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		m.Stop()
		return err
	}
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	m.mu.Lock()
	if !reflect.DeepEqual(cfg, m.cfg) {
		m.generation++
		for conn := range m.connections {
			_ = conn.Close()
			delete(m.connections, conn)
		}
	}
	if m.server != nil && cfg.SocketPath == m.cfg.SocketPath {
		if cfg.SocketMode != m.cfg.SocketMode {
			if err := setSocketMode(cfg.SocketPath, cfg.SocketMode); err != nil {
				old := m.server
				m.server = nil
				m.cfg = cfg
				m.lastError = "无法设置套接字权限：" + err.Error()
				m.mu.Unlock()
				closeServer(old)
				return nil
			}
		}
		m.cfg = cfg
		m.lastError = ""
		m.mu.Unlock()
		return nil
	}
	old := m.server
	m.server = nil
	m.cfg = cfg
	listener, err := net.Listen("unix", cfg.SocketPath)
	if err != nil && removeStaleSocket(cfg.SocketPath) {
		listener, err = net.Listen("unix", cfg.SocketPath)
	}
	if err == nil {
		if err = setSocketMode(cfg.SocketPath, cfg.SocketMode); err != nil {
			_ = listener.Close()
		}
	}
	if err != nil {
		m.lastError = fmt.Sprintf("无法创建套接字 %s：%v", cfg.SocketPath, err)
		m.mu.Unlock()
		if old != nil {
			closeServer(old)
		}
		return nil
	}
	server := &http.Server{Handler: m, ReadHeaderTimeout: 10 * time.Second}
	m.server = server
	m.lastError = ""
	m.mu.Unlock()
	if old != nil {
		closeServer(old)
	}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			m.mu.Lock()
			if m.server == server {
				m.server = nil
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

func closeServer(server *http.Server) {
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
	server := m.server
	m.server = nil
	m.generation++
	for conn := range m.connections {
		_ = conn.Close()
		delete(m.connections, conn)
	}
	m.mu.Unlock()
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
