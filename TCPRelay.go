package relay

import (
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type tcpRelay struct {
	source string
	target string

	listen      net.Listener
	targetAddr  *net.TCPAddr
	connCount   int64
	maxConn     int64
	connTimeout time.Duration
}

func NewTCPRelay() Relay {
	r := tcpRelay{}
	return &r
}

func (t *tcpRelay) Serve(src, dst string, stopCh chan struct{}) error {
	t.source = src
	t.target = dst
	t.maxConn = 10000 // Default max connections
	t.connTimeout = 5 * time.Minute

	// Fix: Cache resolved address
	rAddr, err := net.ResolveTCPAddr("tcp", dst)
	if err != nil {
		return err
	}
	t.targetAddr = rAddr

	log.Printf("Serve TCP: %v => %v", src, dst)
	ln, err := net.Listen("tcp", src)
	if err != nil {
		return err
	}
	t.listen = ln

	// Fix: Close listener when stopCh is signaled
	go func() {
		<-stopCh
		log.Println("Stopping TCP relay:", src)
		t.listen.Close()
	}()

	for {
		conn, err := t.listen.Accept()
		if err != nil {
			// Check if it's due to listener being closed
			select {
			case <-stopCh:
				log.Println("TCP relay stopped gracefully")
				return nil
			default:
				log.Println("Accept error:", err)
				return err
			}
		}

		// Fix: Connection limit
		if atomic.LoadInt64(&t.connCount) >= t.maxConn {
			log.Println("Connection limit reached, rejecting")
			conn.Close()
			continue
		}

		atomic.AddInt64(&t.connCount, 1)
		go t.handleConnection(conn)
	}
}

func (t *tcpRelay) handleConnection(conn net.Conn) error {
	defer atomic.AddInt64(&t.connCount, -1)
	defer conn.Close()

	// Fix: Use cached address
	var remote net.Conn
	var err error

	if t.targetAddr.IP.To4() != nil {
		remote, err = net.DialTimeout("tcp", t.targetAddr.String(), time.Second*5)
	} else {
		remote, err = net.DialTimeout("tcp6", t.targetAddr.String(), time.Second*5)
	}
	if err != nil {
		log.Println("Dial error:", err)
		return err
	}
	defer remote.Close()

	// Fix: Set read/write timeouts
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}
	if tcpRemote, ok := remote.(*net.TCPConn); ok {
		tcpRemote.SetKeepAlive(true)
		tcpRemote.SetKeepAlivePeriod(30 * time.Second)
	}

	// Fix: Use WaitGroup to wait for both goroutines
	var wg sync.WaitGroup
	wg.Add(2)

	// Client -> Server
	go func() {
		defer wg.Done()
		if t.connTimeout > 0 {
			conn.SetReadDeadline(time.Now().Add(t.connTimeout))
			remote.SetWriteDeadline(time.Now().Add(t.connTimeout))
		}
		n, err := io.Copy(remote, conn)
		if err != nil {
			log.Printf("Copy client->server error: %v (bytes: %d)", err, n)
		}
		// Fix: Half-close
		if tcpRemote, ok := remote.(*net.TCPConn); ok {
			tcpRemote.CloseWrite()
		}
	}()

	// Server -> Client
	go func() {
		defer wg.Done()
		if t.connTimeout > 0 {
			remote.SetReadDeadline(time.Now().Add(t.connTimeout))
			conn.SetWriteDeadline(time.Now().Add(t.connTimeout))
		}
		n, err := io.Copy(conn, remote)
		if err != nil {
			log.Printf("Copy server->client error: %v (bytes: %d)", err, n)
		}
		// Fix: Half-close
		if tcpConn, ok := conn.(*net.TCPConn); ok {
			tcpConn.CloseWrite()
		}
	}()

	wg.Wait()
	return nil
}
