// Package client is the side of the daemon protocol that runs inside the
// short-lived `hpcc wrap` / symlink invocation. It lives in its own package
// so the runner can import it without pulling in the daemon server (which
// itself imports runner — putting the client in the daemon package would
// form a cycle).
package client

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/aarani/hpcc/internal/protocol/gen"
	"google.golang.org/protobuf/proto"
)

type Config struct {
	Pid       int
	Port      int
	AuthToken string
}

func ConfigPath() string {
	userConfigDir, err := os.UserConfigDir()
	if err != nil {
		panic(err)
	}
	return filepath.Join(userConfigDir, "hpcc", "daemon.json")
}

// Load returns the daemon config if a daemon process is alive, or nil
// otherwise. A missing/unparseable config file or a dead PID all collapse
// into the same nil result — the caller falls back to local execution.
func Load() *Config {
	bytes, err := os.ReadFile(ConfigPath())
	if err != nil {
		return nil
	}
	cfg := &Config{}
	if err := json.Unmarshal(bytes, cfg); err != nil {
		return nil
	}
	process, err := os.FindProcess(cfg.Pid)
	if err != nil || process == nil {
		return nil
	}
	if err := process.Signal(syscall.Signal(0)); err != nil {
		return nil
	}
	return cfg
}

// Dispatch sends a single CompileRequest to the daemon and reads back
// the response. Length-prefixed protobuf, matching the framing in
// daemon.handleConnection. The connection opens with an auth handshake:
// we send the token from the on-disk config, the server validates it and
// either continues or hard-closes.
func Dispatch(cfg *Config, cwd string, args []string) (*gen.CompileResponse, error) {
	conn, err := net.Dial("tcp", net.JoinHostPort("localhost", strconv.Itoa(cfg.Port)))
	if err != nil {
		return nil, fmt.Errorf("dial daemon: %w", err)
	}
	defer conn.Close()

	if err := writeFrame(conn, []byte(cfg.AuthToken)); err != nil {
		return nil, fmt.Errorf("send auth: %w", err)
	}

	payload, err := proto.Marshal(&gen.CompileRequest{Cwd: cwd, Args: args})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	if err := writeFrame(conn, payload); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	reader := bufio.NewReader(conn)
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, fmt.Errorf("read length: %w", err)
	}
	n := binary.BigEndian.Uint32(header)
	if n == 0 {
		return nil, errors.New("empty response from daemon")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, fmt.Errorf("read payload: %w", err)
	}
	resp := &gen.CompileResponse{}
	if err := proto.Unmarshal(body, resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	return resp, nil
}

func writeFrame(conn net.Conn, payload []byte) error {
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	if _, err := conn.Write(header); err != nil {
		return err
	}
	_, err := conn.Write(payload)
	return err
}
