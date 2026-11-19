package adapter

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tcpTestAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func assertListenerServing(t *testing.T, network, address string) {
	t.Helper()
	conn, err := net.DialTimeout(network, address, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	req, err := http.NewRequest(http.MethodGet, "http://adapter/work/backend-api/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), `"type":"adapter_error"`) {
		t.Fatalf("listener did not enforce API authentication: %d %s %v", resp.StatusCode, body, err)
	}
}

func assertTCPPortFree(t *testing.T, address string) {
	t.Helper()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("old listener still owns %s: %v", address, err)
	}
	_ = listener.Close()
}

func TestListenerModeSwitchReuseAndShutdown(t *testing.T) {
	dir, err := os.MkdirTemp("", "api-listen-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	cfg := testConfig()
	cfg.SocketPath = filepath.Join(dir, "adapter.sock")
	manager := NewManager()
	defer manager.Stop()
	configure := func(network, address string) {
		t.Helper()
		if err := manager.Configure(cfg); err != nil {
			t.Fatal(err)
		}
		status := manager.Status()
		if !status.Running || status.Error != "" || status.ListenMode != network || status.ListenAddress != address {
			t.Fatalf("unexpected listener status: %+v", status)
		}
		assertListenerServing(t, network, address)
	}
	unixAddress := cfg.SocketPath
	configure("unix", unixAddress)
	untouched := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(untouched, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(untouched)
	if err != nil {
		t.Fatal(err)
	}
	firstTCP := tcpTestAddress(t)
	cfg.ListenMode, cfg.ListenAddress = "tcp", firstTCP
	cfg.SocketPath, cfg.SocketMode = untouched, "unused"
	configure("tcp", firstTCP)
	if conn, err := net.DialTimeout("unix", unixAddress, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("old Unix listener still accepts connections")
	}
	cfg.SocketPath = "inactive-relative.sock"
	cfg.ListenAddress = "[::ffff:127.0.0.1]:" + strings.Split(firstTCP, ":")[1]
	configure("tcp", firstTCP)
	contents, err := os.ReadFile(untouched)
	if err != nil || string(contents) != "keep" {
		t.Fatalf("TCP configuration touched the inactive socket path: %q %v", contents, err)
	}
	after, err := os.Stat(untouched)
	if err != nil || after.Mode() != before.Mode() {
		t.Fatalf("TCP configuration changed file permissions: %v", err)
	}
	secondTCP := tcpTestAddress(t)
	cfg.ListenAddress = secondTCP
	configure("tcp", secondTCP)
	assertTCPPortFree(t, firstTCP)
	cfg.ListenMode, cfg.SocketPath, cfg.SocketMode = "unix", unixAddress, "0660"
	configure("unix", unixAddress)
	assertTCPPortFree(t, secondTCP)
	manager.Stop()
	status := manager.Status()
	if status.Running || status.ListenMode != "" || status.ListenAddress != "" {
		t.Fatalf("stopped listener advertised as live: %+v", status)
	}
	listener, err := net.Listen("unix", unixAddress)
	if err != nil {
		t.Fatalf("shutdown did not release Unix socket: %v", err)
	}
	_ = listener.Close()
}

func TestTCPPortConflictStopsListenerAndRecovers(t *testing.T) {
	cfg := testConfig()
	cfg.ListenMode, cfg.ListenAddress = "tcp", tcpTestAddress(t)
	manager := NewManager()
	defer manager.Stop()
	if err := manager.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	firstTCP := cfg.ListenAddress
	assertListenerServing(t, "tcp", firstTCP)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	cfg.ListenAddress = occupied.Addr().String()
	if err := manager.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	status := manager.Status()
	if status.Running || status.ListenMode != "" || status.ListenAddress != "" || !strings.Contains(status.Error, cfg.ListenAddress) {
		t.Fatalf("port conflict not reported as a stopped listener: %+v", status)
	}
	assertTCPPortFree(t, firstTCP)
	_ = occupied.Close()
	if err := manager.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	status = manager.Status()
	if !status.Running || status.Error != "" || status.ListenMode != "tcp" || status.ListenAddress != cfg.ListenAddress {
		t.Fatalf("listener did not recover after freeing port: %+v", status)
	}
	assertListenerServing(t, "tcp", cfg.ListenAddress)
	manager.Stop()
	assertTCPPortFree(t, cfg.ListenAddress)
}

func TestTCPListenerAddressValidation(t *testing.T) {
	for _, address := range []string{"127.0.0.1:18318", "0.0.0.0:18318", "[::1]:18318", "[::]:18318", "[::ffff:127.0.0.1]:18318"} {
		cfg := testConfig()
		cfg.ListenMode, cfg.ListenAddress = "tcp", address
		cfg.SocketPath, cfg.SocketMode = "inactive", "inactive"
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid TCP address %q rejected: %v", address, err)
		}
	}
	for _, address := range []string{"", ":18318", "localhost:18318", "http://127.0.0.1:18318", "127.0.0.1:0", "127.0.0.1:65536", "[fe80::1%eth0]:18318"} {
		cfg := testConfig()
		cfg.ListenMode, cfg.ListenAddress = "tcp", address
		if err := cfg.Validate(); err == nil {
			t.Fatalf("invalid TCP address %q accepted", address)
		}
	}
	cfg := testConfig()
	cfg.ListenMode = "udp"
	if err := cfg.Validate(); err == nil {
		t.Fatal("unsupported listener transport accepted")
	}
}
