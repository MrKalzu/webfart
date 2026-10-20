package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"webfart/internal/storage"
)

// AdminCommand represents an interprocess command sent to admin.sock.
type AdminCommand struct {
	Action string `json:"action"`
	Thread string `json:"thread"`
	Seq    uint64 `json:"seq"`
}

// AdminResponse represents the reply from admin.sock.
type AdminResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// AdminServer listens on a Unix domain socket for administrative control commands.
type AdminServer struct {
	sockPath  string
	listener  net.Listener
	mgr       *storage.Manager
	quit      chan struct{}
	closeOnce sync.Once
}

// NewAdminServer starts the Unix domain socket server.
func NewAdminServer(sockPath string, mgr *storage.Manager) (*AdminServer, error) {
	// Ensure parent directory exists
	dir := filepath.Dir(sockPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed creating socket dir: %w", err)
	}

	// Remove stale socket if present
	_ = os.Remove(sockPath)

	l, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("failed listening on unix socket %s: %w", sockPath, err)
	}
	// Restrict socket permissions to 0600
	_ = os.Chmod(sockPath, 0600)

	as := &AdminServer{
		sockPath: sockPath,
		listener: l,
		mgr:      mgr,
		quit:     make(chan struct{}),
	}

	go as.serve()
	return as, nil
}

func (as *AdminServer) serve() {
	for {
		conn, err := as.listener.Accept()
		if err != nil {
			select {
			case <-as.quit:
				return
			default:
				continue
			}
		}
		go as.handleConn(conn)
	}
}

func (as *AdminServer) handleConn(conn net.Conn) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return
	}
	line := strings.TrimSpace(scanner.Text())

	var cmd AdminCommand
	// Try parsing JSON first
	if strings.HasPrefix(line, "{") {
		if err := json.Unmarshal([]byte(line), &cmd); err != nil {
			sendAdminResp(conn, false, "malformed JSON command: "+err.Error())
			return
		}
	} else {
		// Fallback to space-separated command: "tombstone <thread> <seq>"
		parts := strings.Fields(line)
		if len(parts) >= 3 && parts[0] == "tombstone" {
			seq, err := strconv.ParseUint(parts[2], 10, 64)
			if err != nil {
				sendAdminResp(conn, false, "invalid seq number")
				return
			}
			cmd = AdminCommand{
				Action: "tombstone",
				Thread: parts[1],
				Seq:    seq,
			}
		} else {
			sendAdminResp(conn, false, "unknown command format")
			return
		}
	}

	switch cmd.Action {
	case "tombstone":
		if err := as.mgr.Tombstone(cmd.Thread, cmd.Seq); err != nil {
			sendAdminResp(conn, false, err.Error())
			return
		}
		sendAdminResp(conn, true, "")
	default:
		sendAdminResp(conn, false, "unsupported action: "+cmd.Action)
	}
}

func sendAdminResp(conn net.Conn, ok bool, errMsg string) {
	resp := AdminResponse{OK: ok, Error: errMsg}
	data, _ := json.Marshal(resp)
	_, _ = conn.Write(append(data, '\n'))
}

// Close terminates the Unix domain socket server and removes the socket file idempotently.
func (as *AdminServer) Close() {
	as.closeOnce.Do(func() {
		close(as.quit)
		if as.listener != nil {
			_ = as.listener.Close()
		}
		_ = os.Remove(as.sockPath)
	})
}
