// Package testssh is an in-process SSH fixture used only by tests and local QA.
// Its shell echoes input; it never executes operating-system commands.
package testssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/ssh"
)

const Banner = "Dozor SSH test fixture\r\n[root@test ~]# "

type Size struct{ Cols, Rows uint32 }

type Server struct {
	Listener    net.Listener
	Signer      ssh.Signer
	Inputs      chan []byte
	Sizes       chan Size
	Attempts    atomic.Int32
	mu          sync.Mutex
	connections map[net.Conn]struct{}
	closed      bool
	wg          sync.WaitGroup
}

func Start(address, password string, extraHostKeys ...ssh.Signer) (*Server, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	s := &Server{Listener: listener, Signer: signer, Inputs: make(chan []byte, 64), Sizes: make(chan Size, 16), connections: make(map[net.Conn]struct{})}
	config := &ssh.ServerConfig{PasswordCallback: func(metadata ssh.ConnMetadata, value []byte) (*ssh.Permissions, error) {
		s.Attempts.Add(1)
		if metadata.User() != "root" || subtle.ConstantTimeCompare(value, []byte(password)) != 1 {
			return nil, errors.New("authentication failed")
		}
		return nil, nil
	}}
	config.AddHostKey(signer)
	for _, extra := range extraHostKeys {
		config.AddHostKey(extra)
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				_ = connection.Close()
				return
			}
			s.connections[connection] = struct{}{}
			s.mu.Unlock()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer connection.Close()
				defer func() { s.mu.Lock(); delete(s.connections, connection); s.mu.Unlock() }()
				sshConn, channels, requests, err := ssh.NewServerConn(connection, config)
				if err != nil {
					return
				}
				defer sshConn.Close()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					if incoming.ChannelType() != "session" {
						_ = incoming.Reject(ssh.UnknownChannelType, "only session channels are supported")
						continue
					}
					channel, requests, err := incoming.Accept()
					if err != nil {
						return
					}
					s.serveShell(channel, requests)
				}
			}()
		}
	}()
	return s, nil
}

func (s *Server) serveShell(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	ready := make(chan struct{})
	go func() {
		for request := range requests {
			switch request.Type {
			case "pty-req":
				var pty struct {
					Term                      string
					Cols, Rows, Width, Height uint32
					Modes                     string
				}
				valid := ssh.Unmarshal(request.Payload, &pty) == nil
				if valid {
					select {
					case s.Sizes <- Size{pty.Cols, pty.Rows}:
					default:
					}
				}
				_ = request.Reply(valid, nil)
			case "window-change":
				var size struct{ Cols, Rows, Width, Height uint32 }
				if ssh.Unmarshal(request.Payload, &size) == nil {
					select {
					case s.Sizes <- Size{size.Cols, size.Rows}:
					default:
					}
				}
			case "shell":
				_ = request.Reply(true, nil)
				close(ready)
			default:
				_ = request.Reply(false, nil)
			}
		}
	}()
	select {
	case <-ready:
	case <-channelDone(channel):
		return
	}
	_, _ = io.WriteString(channel, Banner)
	buffer := make([]byte, 8192)
	var command strings.Builder
	for {
		n, err := channel.Read(buffer)
		if err != nil {
			return
		}
		input := append([]byte(nil), buffer[:n]...)
		select {
		case s.Inputs <- input:
		default:
		}
		_, _ = channel.Write(input)
		for _, value := range input {
			if value == 3 {
				command.Reset()
				_, _ = io.WriteString(channel, "^C\r\n[root@test ~]# ")
				continue
			}
			if value == '\r' || value == '\n' {
				if command.String() == "exit" {
					_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
					return
				}
				_, _ = io.WriteString(channel, "\r\nfixture: "+command.String()+"\r\n[root@test ~]# ")
				command.Reset()
			} else {
				_ = command.WriteByte(value)
			}
		}
	}
}

// A shell request is always sent before application data in these fixtures.
// Waiting on channel stderr avoids consuming stdin while the request is pending.
func channelDone(channel ssh.Channel) <-chan struct{} {
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, channel.Stderr()); close(done) }()
	return done
}

func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	_ = s.Listener.Close()
	for connection := range s.connections {
		_ = connection.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}
