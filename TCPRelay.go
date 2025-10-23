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

	listen        net.Listener
	targetAddr    atomic.Value // stores *net.TCPAddr
	connCount     int64
	maxConn       int64
	connTimeout   time.Duration
	dnsRefresh    time.Duration
	failureCount  int64
	lastDNSUpdate time.Time
	addrMu        sync.RWMutex
}

func NewTCPRelay() Relay {
	r := tcpRelay{}
	return &r
}

func (t *tcpRelay) Serve(src, dst string, stopCh chan struct{}) error {
	t.source = src
	t.target = dst
	t.maxConn = 10000          // Default max connections
	t.connTimeout = 5 * time.Minute
	t.dnsRefresh = 5 * time.Minute // Refresh DNS every 5 minutes

	// Initial DNS resolution
	if err := t.refreshDNS(); err != nil {
		return err
	}

	log.Printf("Serve TCP: %v => %v (resolved to %v)", src, dst, t.getTargetAddr())

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

	// DNS refresh goroutine
	go t.dnsRefreshLoop(stopCh)

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

	// Get current target address
	targetAddr := t.getTargetAddr()
	var remote net.Conn
	var err error

	if targetAddr.IP.To4() != nil {
		remote, err = net.DialTimeout("tcp", targetAddr.String(), time.Second*5)
	} else {
		remote, err = net.DialTimeout("tcp6", targetAddr.String(), time.Second*5)
	}
	if err != nil {
		log.Printf("Dial error to %v: %v", targetAddr, err)

		// Track failures and trigger DNS refresh if needed
		failures := atomic.AddInt64(&t.failureCount, 1)
		if failures >= 3 {
			log.Printf("Multiple connection failures (%d), triggering DNS refresh", failures)
			go func() {
				if err := t.refreshDNS(); err == nil {
					atomic.StoreInt64(&t.failureCount, 0)
				}
			}()
		}
		return err
	}

	// Reset failure count on successful connection
	atomic.StoreInt64(&t.failureCount, 0)
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

// refreshDNS re-resolves the target address
func (t *tcpRelay) refreshDNS() error {
	rAddr, err := net.ResolveTCPAddr("tcp", t.target)
	if err != nil {
		log.Printf("DNS resolution failed for %s: %v", t.target, err)
		return err
	}

	t.addrMu.Lock()
	oldAddr := t.getTargetAddr()
	t.targetAddr.Store(rAddr)
	t.lastDNSUpdate = time.Now()
	t.addrMu.Unlock()

	if oldAddr == nil || oldAddr.String() != rAddr.String() {
		log.Printf("DNS updated for %s: %v -> %v", t.target, oldAddr, rAddr)
	}
	return nil
}

// getTargetAddr safely retrieves the current target address
func (t *tcpRelay) getTargetAddr() *net.TCPAddr {
	if addr := t.targetAddr.Load(); addr != nil {
		return addr.(*net.TCPAddr)
	}
	return nil
}

// dnsRefreshLoop periodically refreshes DNS
func (t *tcpRelay) dnsRefreshLoop(stopCh chan struct{}) {
	ticker := time.NewTicker(t.dnsRefresh)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			t.addrMu.RLock()
			elapsed := time.Since(t.lastDNSUpdate)
			t.addrMu.RUnlock()

			if elapsed >= t.dnsRefresh {
				if err := t.refreshDNS(); err == nil {
					log.Printf("Periodic DNS refresh completed for %s", t.target)
				}
			}
		}
	}
}
