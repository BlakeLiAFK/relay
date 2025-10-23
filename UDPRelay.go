package relay

import (
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var (
	DefaultMTU = 1500
)

type udpRelay struct {
	source        string
	target        string
	remote        map[string]*packet
	remoteMu      sync.RWMutex
	targetAddr    atomic.Value // stores *net.UDPAddr
	listener      net.PacketConn
	pktCount      int64
	idleTimeout   time.Duration
	mtu           int
	dnsRefresh    time.Duration
	failureCount  int64
	lastDNSUpdate time.Time
	addrMu        sync.RWMutex
}

func NewUDPRelay() Relay {
	r := udpRelay{}
	return &r
}

type packet struct {
	conn       net.Addr
	data       []byte
	pc         net.PacketConn
	remote     *net.UDPConn
	lastActive time.Time
}

func (t *udpRelay) Serve(src, dst string, stopCh chan struct{}) error {
	t.source = src
	t.target = dst
	t.remote = make(map[string]*packet)
	t.idleTimeout = 5 * time.Minute
	t.mtu = DefaultMTU
	t.dnsRefresh = 5 * time.Minute // Refresh DNS every 5 minutes

	// Initial DNS resolution
	if err := t.refreshDNS(); err != nil {
		return err
	}

	log.Printf("Serve UDP: %v => %v (resolved to %v)", src, dst, t.getTargetAddr())

	ln, err := net.ListenPacket("udp", src)
	if err != nil {
		log.Println(err)
		return err
	}
	t.listener = ln

	// Fix: Close listener when stopCh is signaled
	go func() {
		<-stopCh
		log.Println("Stopping UDP relay:", src)
		t.cleanup()
		ln.Close()
	}()

	// Fix: Idle connection cleanup
	go t.cleanupIdle()

	// DNS refresh goroutine
	go t.dnsRefreshLoop(stopCh)

	buf := make([]byte, t.mtu)
	for {
		n, conn, err := ln.ReadFrom(buf)
		if err != nil {
			// Check if it's due to listener being closed
			select {
			case <-stopCh:
				log.Println("UDP relay stopped gracefully")
				return nil
			default:
				log.Println("ReadFrom error:", err)
				return err
			}
		}

		atomic.AddInt64(&t.pktCount, 1)

		// Fix: Copy data to avoid race condition
		data := make([]byte, n)
		copy(data, buf[:n])

		connKey := conn.String()
		t.remoteMu.RLock()
		pkt, ok := t.remote[connKey]
		t.remoteMu.RUnlock()

		if ok {
			pkt.data = data
			pkt.lastActive = time.Now()
			go t.handlePacket(pkt)
		} else {
			pkt = &packet{conn: conn, data: data, pc: ln, lastActive: time.Now()}
			t.remoteMu.Lock()
			t.remote[connKey] = pkt
			t.remoteMu.Unlock()
			go t.handlePacket(pkt)
		}
	}
}

func (t *udpRelay) handlePacket(p *packet) {
	if p.remote == nil {
		var remote *net.UDPConn
		var err error

		// Get current target address
		targetAddr := t.getTargetAddr()
		if targetAddr.IP.To4() != nil {
			remote, err = net.DialUDP("udp", nil, targetAddr)
		} else {
			remote, err = net.DialUDP("udp6", nil, targetAddr)
		}
		if err != nil {
			log.Printf("DialUDP error to %v: %v", targetAddr, err)

			// Track failures and trigger DNS refresh if needed
			failures := atomic.AddInt64(&t.failureCount, 1)
			if failures >= 3 {
				log.Printf("Multiple UDP dial failures (%d), triggering DNS refresh", failures)
				go func() {
					if err := t.refreshDNS(); err == nil {
						atomic.StoreInt64(&t.failureCount, 0)
					}
				}()
			}
			t.die(p)
			return
		}

		// Reset failure count on successful dial
		atomic.StoreInt64(&t.failureCount, 0)
		p.remote = remote

		// Start reading from remote
		go func() {
			buf := make([]byte, t.mtu)
			for {
				// Set read deadline for idle cleanup
				if t.idleTimeout > 0 {
					remote.SetReadDeadline(time.Now().Add(t.idleTimeout))
				}

				n, err := remote.Read(buf)
				if err != nil {
					log.Println("Remote read error:", err)
					t.die(p)
					return
				}

				p.lastActive = time.Now()
				data := make([]byte, n)
				copy(data, buf[:n])

				_, err = p.pc.WriteTo(data, p.conn)
				if err != nil {
					log.Println("WriteTo error:", err)
					t.die(p)
					return
				}
			}
		}()
	}

	_, err := p.remote.Write(p.data)
	if err != nil {
		log.Println("Remote write error:", err)
		t.die(p)
	}
}

func (t *udpRelay) die(p *packet) {
	log.Println("Disconnecting UDP session:", p.conn.String())
	// Fix: Close remote connection to prevent leak
	if p.remote != nil {
		p.remote.Close()
	}
	t.remoteMu.Lock()
	delete(t.remote, p.conn.String())
	t.remoteMu.Unlock()
}

// Fix: Cleanup idle connections
func (t *udpRelay) cleanupIdle() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		t.remoteMu.Lock()
		for key, pkt := range t.remote {
			if now.Sub(pkt.lastActive) > t.idleTimeout {
				log.Printf("Cleaning up idle UDP session: %s (idle for %v)", key, now.Sub(pkt.lastActive))
				if pkt.remote != nil {
					pkt.remote.Close()
				}
				delete(t.remote, key)
			}
		}
		t.remoteMu.Unlock()
	}
}

// Fix: Cleanup all connections
func (t *udpRelay) cleanup() {
	t.remoteMu.Lock()
	defer t.remoteMu.Unlock()

	log.Printf("Cleaning up %d UDP sessions", len(t.remote))
	for _, pkt := range t.remote {
		if pkt.remote != nil {
			pkt.remote.Close()
		}
	}
	t.remote = make(map[string]*packet)
}

// refreshDNS re-resolves the target address
func (t *udpRelay) refreshDNS() error {
	rAddr, err := net.ResolveUDPAddr("udp", t.target)
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
func (t *udpRelay) getTargetAddr() *net.UDPAddr {
	if addr := t.targetAddr.Load(); addr != nil {
		return addr.(*net.UDPAddr)
	}
	return nil
}

// dnsRefreshLoop periodically refreshes DNS
func (t *udpRelay) dnsRefreshLoop(stopCh chan struct{}) {
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
