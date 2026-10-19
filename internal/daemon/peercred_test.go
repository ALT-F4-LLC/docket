package daemon

import (
	"net"
	"os"
	"testing"
)

// unixPair returns both ends of a same-process connection to the daemon socket:
// the dialed client end and the end the listener accepted.
func unixPair(t *testing.T) (dialed, accepted *net.UnixConn) {
	t.Helper()
	shortRuntimeBase(t)
	ln, err := Listen(mustEndpoint(t, newStoreDir(t)))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	type acceptResult struct {
		conn net.Conn
		err  error
	}
	acceptCh := make(chan acceptResult, 1)
	go func() {
		conn, err := ln.Accept()
		acceptCh <- acceptResult{conn, err}
	}()

	dialed, err = net.DialUnix("unix", nil, ln.Addr().(*net.UnixAddr))
	if err != nil {
		t.Fatalf("DialUnix: %v", err)
	}
	t.Cleanup(func() { dialed.Close() })
	r := <-acceptCh
	if r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}
	t.Cleanup(func() { r.conn.Close() })
	return dialed, r.conn.(*net.UnixConn)
}

func TestPeerUIDReportsCurrentUIDOnBothEnds(t *testing.T) {
	dialed, accepted := unixPair(t)
	for _, end := range []struct {
		name string
		conn *net.UnixConn
	}{{"accepted", accepted}, {"dialed", dialed}} {
		uid, err := peerUID(end.conn)
		if err != nil {
			t.Errorf("peerUID on the %s end: %v", end.name, err)
			continue
		}
		if int(uid) != os.Getuid() {
			t.Errorf("peerUID on the %s end = %d, want the current uid %d", end.name, uid, os.Getuid())
		}
	}
}

func TestPeerUIDFailsOnClosedConn(t *testing.T) {
	_, accepted := unixPair(t)
	accepted.Close()
	if uid, err := peerUID(accepted); err == nil {
		t.Fatalf("peerUID on a closed conn = %d with no error, want an error", uid)
	}
}
