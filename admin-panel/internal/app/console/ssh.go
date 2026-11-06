package console

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type ConnectRequest struct {
	Host             string `json:"host"`
	Port             int    `json:"port"`
	User             string `json:"user"`
	Password         string `json:"password"`
	Cols             int    `json:"cols"`
	Rows             int    `json:"rows"`
	TrustFingerprint string `json:"trust_fingerprint,omitempty"`
}

func (r *ConnectRequest) Validate() error {
	r.Host = strings.TrimSpace(strings.Trim(r.Host, "[]"))
	r.User = strings.TrimSpace(r.User)
	if r.Port == 0 {
		r.Port = 22
	}
	if r.Host == "" || len(r.Host) > 253 || strings.ContainsAny(r.Host, " /\\@\t\r\n\x00") || r.Port < 1 || r.Port > 65535 {
		return errors.New("Укажите IP или имя камеры и SSH-порт от 1 до 65535")
	}
	if r.User == "" || len(r.User) > 64 || strings.ContainsAny(r.User, "\r\n\x00") {
		return errors.New("Укажите SSH-пользователя камеры")
	}
	if r.Password == "" || len(r.Password) > 4096 || len(r.TrustFingerprint) > 128 {
		return errors.New("Введите пароль SSH")
	}
	return ValidateSize(r.Cols, r.Rows)
}

func ValidateSize(cols, rows int) error {
	if cols < 2 || cols > 500 || rows < 2 || rows > 200 {
		return errors.New("Размер терминала должен быть от 2×2 до 500×200")
	}
	return nil
}

type Remote interface {
	Input() io.Writer
	Output() io.Reader
	Errors() io.Reader
	Resize(cols, rows int) error
	KeepAlive() error
	Wait() error
	Close() error
}

type Connector struct{ Keys *HostKeys }

func (c *Connector) Connect(ctx context.Context, request ConnectRequest) (Remote, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if c.Keys == nil {
		return nil, errors.New("Хранилище SSH-ключей не настроено")
	}
	address := net.JoinHostPort(request.Host, strconv.Itoa(request.Port))
	algorithms, err := c.Keys.algorithms(address)
	if err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	connection, err := (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext(dialCtx, "tcp", address)
	if err != nil {
		return nil, errors.New("Не удалось подключиться к SSH камеры. Проверьте адрес, питание и порт")
	}
	closeOnCancel := context.AfterFunc(dialCtx, func() { _ = connection.Close() })
	defer closeOnCancel()
	_ = connection.SetDeadline(time.Now().Add(12 * time.Second))
	config := &ssh.ClientConfig{User: request.User, Auth: []ssh.AuthMethod{ssh.Password(request.Password)}, HostKeyCallback: c.Keys.callback(request.TrustFingerprint), HostKeyAlgorithms: algorithms, ClientVersion: "SSH-2.0-DozorConsole"}
	sshConnection, channels, requests, err := ssh.NewClientConn(connection, address, config)
	request.Password = ""
	config.Auth = nil
	if err != nil {
		_ = connection.Close()
		var keyError *HostKeyError
		if errors.As(err, &keyError) {
			return nil, keyError
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, errors.New("Неверный SSH-пользователь или пароль")
		}
		return nil, errors.New("SSH-согласование не завершилось. Проверьте доступность камеры и её SSH-сервер")
	}
	_ = connection.SetDeadline(time.Time{})
	client := ssh.NewClient(sshConnection, channels, requests)
	session, err := client.NewSession()
	if err != nil {
		_ = client.Close()
		return nil, errors.New("Камера не смогла открыть SSH-сеанс")
	}
	remote := &sessionRemote{client: client, session: session}
	fail := func() (Remote, error) {
		_ = remote.Close()
		return nil, errors.New("Камера не смогла открыть интерактивный терминал")
	}
	if remote.input, err = session.StdinPipe(); err != nil {
		return fail()
	}
	if remote.output, err = session.StdoutPipe(); err != nil {
		return fail()
	}
	if remote.errors, err = session.StderrPipe(); err != nil {
		return fail()
	}
	if err = session.RequestPty("xterm-256color", request.Rows, request.Cols, ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}); err != nil {
		return fail()
	}
	if err = session.Shell(); err != nil {
		return fail()
	}
	return remote, nil
}

type sessionRemote struct {
	client  *ssh.Client
	session *ssh.Session
	input   io.WriteCloser
	output  io.Reader
	errors  io.Reader
	once    sync.Once
}

func (s *sessionRemote) Input() io.Writer  { return s.input }
func (s *sessionRemote) Output() io.Reader { return s.output }
func (s *sessionRemote) Errors() io.Reader { return s.errors }
func (s *sessionRemote) Wait() error       { return s.session.Wait() }
func (s *sessionRemote) Resize(cols, rows int) error {
	if err := ValidateSize(cols, rows); err != nil {
		return err
	}
	return s.session.WindowChange(rows, cols)
}
func (s *sessionRemote) KeepAlive() error {
	_, _, err := s.client.SendRequest("keepalive@openssh.com", true, nil)
	return err
}
func (s *sessionRemote) Close() error {
	s.once.Do(func() {
		_ = s.session.Close()
		_ = s.client.Close()
	})
	return nil
}

func ExitMessage(err error) string {
	if err == nil {
		return "SSH-сеанс завершён"
	}
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		return fmt.Sprintf("Оболочка завершилась с кодом %d", exit.ExitStatus())
	}
	return "Соединение с камерой прервано"
}
