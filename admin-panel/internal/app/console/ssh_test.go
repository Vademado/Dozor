package console

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"guard.local/admin/internal/testssh"
)

func testKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

func TestHostKeysRequireTrustAndRejectChangedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh_known_hosts")
	store := NewHostKeys(path)
	key, other := testKey(t), testKey(t)
	address := "127.0.0.1:22"
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
	var unknown *HostKeyError
	if err := store.callback("")(address, remote, key); !errors.As(err, &unknown) || unknown.Changed {
		t.Fatal("unknown host was not challenged", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unconfirmed key was written")
	}
	if err := store.callback(ssh.FingerprintSHA256(key))(address, remote, key); err != nil {
		t.Fatal(err)
	}
	if err := store.callback("")(address, remote, key); err != nil {
		t.Fatal("saved key not accepted", err)
	}
	before, _ := os.ReadFile(path)
	var changed *HostKeyError
	if err := store.callback(ssh.FingerprintSHA256(other))(address, remote, other); !errors.As(err, &changed) || !changed.Changed {
		t.Fatal("changed host key accepted", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("changed key rewrote known_hosts")
	}
}

func TestExistingSystemHostKeysAreReadOnly(t *testing.T) {
	dir := t.TempDir()
	external := filepath.Join(dir, "external")
	privateStore := filepath.Join(dir, "app")
	key := testKey(t)
	line := knownhosts.Line([]string{"127.0.0.1:22"}, key) + "\n"
	if err := os.WriteFile(external, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewHostKeys(privateStore, external)
	if err := store.callback("")("127.0.0.1:22", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}, key); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(external); string(data) != line {
		t.Fatal("external keys were modified")
	}
	if _, err := os.Stat(privateStore); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("known external key was unnecessarily copied")
	}
}

func TestSSHAuthenticationPTYResizeAndInput(t *testing.T) {
	server, err := testssh.Start("127.0.0.1:0", "fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	host, portString, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portString)
	knownPath := filepath.Join(t.TempDir(), "ssh_known_hosts")
	connector := &Connector{Keys: NewHostKeys(knownPath)}
	request := ConnectRequest{Host: host, Port: port, User: "root", Password: "fixture-password", Cols: 100, Rows: 30}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var keyError *HostKeyError
	if _, err = connector.Connect(ctx, request); !errors.As(err, &keyError) {
		t.Fatal("first-use challenge missing", err)
	}
	if server.Attempts.Load() != 0 {
		t.Fatal("password was sent before host-key verification")
	}
	request.TrustFingerprint = ssh.FingerprintSHA256(server.Signer.PublicKey())
	remote, err := connector.Connect(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	stop := context.AfterFunc(ctx, func() { _ = remote.Close() })
	defer stop()
	banner := make([]byte, len(testssh.Banner))
	if _, err = io.ReadFull(remote.Output(), banner); err != nil || string(banner) != testssh.Banner {
		t.Fatal("PTY output missing", err)
	}
	select {
	case size := <-server.Sizes:
		if size.Cols != 100 || size.Rows != 30 {
			t.Fatal("wrong PTY size")
		}
	case <-ctx.Done():
		t.Fatal("missing PTY")
	}
	if err = remote.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	select {
	case size := <-server.Sizes:
		if size.Cols != 120 || size.Rows != 40 {
			t.Fatal("wrong resized PTY")
		}
	case <-ctx.Done():
		t.Fatal("missing resize")
	}
	want := "echo привет\r\x03"
	if _, err = io.WriteString(remote.Input(), want); err != nil {
		t.Fatal(err)
	}
	select {
	case input := <-server.Inputs:
		if string(input) != want {
			t.Fatalf("input changed: %q", input)
		}
	case <-ctx.Done():
		t.Fatal("missing stdin")
	}
	if err = remote.KeepAlive(); err != nil {
		t.Fatal(err)
	}
	request.Password = "wrong-password"
	if _, err = connector.Connect(ctx, request); err == nil || strings.Contains(err.Error(), request.Password) {
		t.Fatal("bad password accepted or leaked", err)
	}
	data, _ := os.ReadFile(knownPath)
	if strings.Contains(string(data), "fixture-password") {
		t.Fatal("password persisted")
	}
}

func TestHandshakeCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() { connection, _ := listener.Accept(); accepted <- connection }()
	host, portString, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portString)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (&Connector{Keys: NewHostKeys(filepath.Join(t.TempDir(), "known"))}).Connect(ctx, ConnectRequest{Host: host, Port: port, User: "root", Password: "test", Cols: 80, Rows: 24})
		done <- err
	}()
	select {
	case connection := <-accepted:
		defer connection.Close()
	case <-time.After(time.Second):
		t.Fatal("connection not made")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled handshake succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not interrupt SSH handshake")
	}
}

func TestSSHUsesKnownEd25519WhenCameraAlsoOffersECDSA(t *testing.T) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecdsaKey, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	server, err := testssh.Start("127.0.0.1:0", "test-password", ecdsaKey)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	address := server.Listener.Addr().String()
	dir := t.TempDir()
	external := filepath.Join(dir, "external_known_hosts")
	known := knownhosts.Line([]string{address}, server.Signer.PublicKey()) + "\n"
	if err = os.WriteFile(external, []byte(known), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewHostKeys(filepath.Join(dir, "app_known_hosts"), external)
	algorithms, err := store.algorithms(address)
	if err != nil || len(algorithms) == 0 || algorithms[0] != ssh.KeyAlgoED25519 {
		t.Fatal("trusted algorithm not preferred", algorithms, err)
	}
	host, portString, _ := net.SplitHostPort(address)
	port, _ := strconv.Atoi(portString)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	remote, err := (&Connector{Keys: store}).Connect(ctx, ConnectRequest{Host: host, Port: port, User: "root", Password: "test-password", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal("known Ed25519 key was rejected in favor of ECDSA", err)
	}
	_ = remote.Close()
	if data, _ := os.ReadFile(external); string(data) != known {
		t.Fatal("system known_hosts modified")
	}
}
