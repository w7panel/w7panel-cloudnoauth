package listener

import (
	"bufio"
	"errors"
	"net"
	"sync"
	"time"
)

const (
	fastCGIVersion1          = 1
	protocolDetectionTimeout = 10 * time.Second
)

// SplitHTTPAndFastCGI splits one TCP listener into HTTP and FastCGI
// listeners. FastCGI records always start with protocol version byte 1; all
// other connections are handed to net/http so it can return its normal parse
// errors for malformed traffic.
func SplitHTTPAndFastCGI(listener net.Listener) (net.Listener, net.Listener) {
	httpListener := newDispatchedListener(listener.Addr())
	fastCGIListener := newDispatchedListener(listener.Addr())
	go dispatchConnections(listener, httpListener, fastCGIListener)
	return httpListener, fastCGIListener
}

func dispatchConnections(listener net.Listener, httpListener, fastCGIListener *dispatchedListener) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			httpListener.setError(err)
			fastCGIListener.setError(err)
			return
		}

		reader := bufio.NewReader(connection)
		_ = connection.SetReadDeadline(time.Now().Add(protocolDetectionTimeout))
		prefix, err := reader.Peek(1)
		_ = connection.SetReadDeadline(time.Time{})
		if err != nil {
			_ = connection.Close()
			continue
		}

		target := httpListener
		if prefix[0] == fastCGIVersion1 {
			target = fastCGIListener
		}
		if !target.dispatch(&bufferedConn{Conn: connection, reader: reader}) {
			_ = connection.Close()
		}
	}
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (connection *bufferedConn) Read(buffer []byte) (int, error) {
	return connection.reader.Read(buffer)
}

type dispatchedListener struct {
	address     net.Addr
	connections chan net.Conn
	done        chan struct{}
	errors      chan error
	closeOnce   sync.Once
	errorOnce   sync.Once
}

func newDispatchedListener(address net.Addr) *dispatchedListener {
	return &dispatchedListener{
		address:     address,
		connections: make(chan net.Conn),
		done:        make(chan struct{}),
		errors:      make(chan error, 1),
	}
}

func (listener *dispatchedListener) Accept() (net.Conn, error) {
	select {
	case connection := <-listener.connections:
		return connection, nil
	case err := <-listener.errors:
		return nil, err
	case <-listener.done:
		return nil, net.ErrClosed
	}
}

func (listener *dispatchedListener) Close() error {
	listener.closeOnce.Do(func() {
		close(listener.done)
	})
	return nil
}

func (listener *dispatchedListener) Addr() net.Addr {
	return listener.address
}

func (listener *dispatchedListener) dispatch(connection net.Conn) bool {
	select {
	case listener.connections <- connection:
		return true
	case <-listener.done:
		return false
	}
}

func (listener *dispatchedListener) setError(err error) {
	if err == nil {
		err = errors.New("listener dispatcher stopped")
	}
	listener.errorOnce.Do(func() {
		listener.errors <- err
	})
}
