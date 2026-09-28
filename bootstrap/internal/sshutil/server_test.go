package sshutil

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// testServer is a local SSH server that answers every exec with output and
// exit status 0, or, when hang is set, accepts the exec and never answers,
// the way a long image pull looks from the client side.
type testServer struct {
	listener net.Listener
	output   string
	hang     bool

	started chan struct{} // closed when the first exec arrives
	once    sync.Once
	release chan struct{}
}

func newTestServer(t *testing.T, output string, hang bool) *testServer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(key)
	require.NoError(t, err)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &testServer{listener: l, output: output, hang: hang, started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		close(s.release)
		_ = l.Close()
	})

	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go s.handle(conn, cfg)
		}
	}()
	return s
}

func (s *testServer) handle(netConn net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(netConn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, requests, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = ch.Close() }()
			for req := range requests {
				if req.Type != "exec" {
					_ = req.Reply(req.WantReply, nil)
					continue
				}
				_ = req.Reply(true, nil)
				s.once.Do(func() { close(s.started) })
				if s.hang {
					<-s.release
					return
				}
				_, _ = ch.Write([]byte(s.output))
				status := make([]byte, 4)
				binary.BigEndian.PutUint32(status, 0)
				_, _ = ch.SendRequest("exit-status", false, status)
				return
			}
		}()
	}
}

func (s *testServer) client() *Client {
	c := NewClient("vllm", "127.0.0.1", true)
	_, c.port, _ = net.SplitHostPort(s.listener.Addr().String())
	return c
}
