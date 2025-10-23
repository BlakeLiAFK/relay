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
	source      string
	target      string
	remote      map[string]*packet
	remoteMu    sync.RWMutex
	targetAddr  *net.UDPAddr
	listener    net.PacketConn
	pktCount    int64
	idleTimeout time.Duration
	mtu         int
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

	// Fix: Cache resolved address
	rAddr, err := net.ResolveUDPAddr("udp", dst)
	if err != nil {
		return err
	}
	t.targetAddr = rAddr

	log.Printf("Serve UDP: %v => %v", src, dst)
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

		// Fix: Use cached address
		if t.targetAddr.IP.To4() != nil {
			remote, err = net.DialUDP("udp", nil, t.targetAddr)
		} else {
			remote, err = net.DialUDP("udp6", nil, t.targetAddr)
		}
		if err != nil {
			log.Println("DialUDP error:", err)
			t.die(p)
			return
		}
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
