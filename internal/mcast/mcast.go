package mcast

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
)

// Fragment header layout (big-endian):
// 1 byte version (1)
// 4 bytes frameID
// 2 bytes totalFragments
// 2 bytes fragmentIndex
const (
	fragHeaderSize = 1 + 4 + 2 + 2
	fragVersion    = 1
)

type Sender struct {
	conn    *net.UDPConn
	pc      *ipv4.PacketConn
	mu      sync.Mutex
	frameID uint32
}

// NewSender creates a UDP sender to the multicast address. If ifname is empty
// it uses the system default interface. ttl controls multicast TTL (1 is local LAN).
func NewSender(addr string, ifname string, ttl int) (*Sender, error) {
	udpAddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return nil, err
	}

	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return nil, err
	}

	pc := ipv4.NewPacketConn(conn)
	if err := pc.SetMulticastTTL(ttl); err != nil {
		// best-effort; continue
	}
	// allow local loopback so sender on same host can be received by receiver
	_ = pc.SetMulticastLoopback(true)
	if ifname != "" {
		ifi, err := net.InterfaceByName(ifname)
		if err == nil {
			_ = pc.SetMulticastInterface(ifi)
		}
	}

	return &Sender{conn: conn, pc: pc}, nil
}

// SendFrame fragments the frame into MTU-sized packets (accounting for header)
// and sends each fragment. repeats controls how many times each fragment is sent
// (simple redundancy). mtu should be <= 65507.
func (s *Sender) SendFrame(b []byte, mtu int, repeats int) error {
	if mtu <= fragHeaderSize+16 {
		mtu = 1200
	}
	if mtu > 65507 {
		mtu = 65507
	}

	payloadPer := mtu - fragHeaderSize
	if payloadPer <= 0 {
		payloadPer = 1200
	}

	s.mu.Lock()
	s.frameID++
	frameID := s.frameID
	s.mu.Unlock()

	total := (len(b) + payloadPer - 1) / payloadPer

	for i := 0; i < total; i++ {
		start := i * payloadPer
		end := start + payloadPer
		if end > len(b) {
			end = len(b)
		}
		frag := make([]byte, fragHeaderSize+(end-start))
		frag[0] = fragVersion
		binary.BigEndian.PutUint32(frag[1:5], frameID)
		binary.BigEndian.PutUint16(frag[5:7], uint16(total))
		binary.BigEndian.PutUint16(frag[7:9], uint16(i))
		copy(frag[fragHeaderSize:], b[start:end])

		for r := 0; r < repeats; r++ {
			if _, err := s.conn.Write(frag); err != nil {
				return err
			}
			// tiny spacing to avoid bursts
			time.Sleep(1 * time.Millisecond)
		}
	}
	return nil
}

// Backwards-compatible Send: if frame fits in one UDP packet, send with 4-byte length prefix.
func (s *Sender) Send(b []byte) error {
	if len(b)+4 <= 65507 {
		p := make([]byte, 4+len(b))
		p[0] = byte(len(b) >> 24)
		p[1] = byte(len(b) >> 16)
		p[2] = byte(len(b) >> 8)
		p[3] = byte(len(b))
		copy(p[4:], b)
		_, err := s.conn.Write(p)
		return err
	}
	// fallback: use SendFrame with defaults
	return s.SendFrame(b, 1200, 1)
}

func (s *Sender) Close() error {
	if s.pc != nil {
		_ = s.pc.Close()
	}
	if s.conn != nil {
		return s.conn.Close()
	}
	return nil
}

type Receiver struct {
	conn *net.UDPConn
	buf  []byte

	mu     sync.Mutex
	frames map[uint32]*assemblingFrame
	out    chan []byte
	stop   chan struct{}
}

type assemblingFrame struct {
	total    uint16
	parts    map[uint16][]byte
	received int
	created  time.Time
}

// NewReceiver joins the multicast group at addr (e.g. 224.0.0.250:5000). If ifname
// is non-empty it uses that interface, otherwise it picks the first multicast-capable interface.
func NewReceiver(addr string, ifname string) (*Receiver, error) {
	parts := strings.Split(addr, ":")
	if len(parts) != 2 {
		return nil, fmt.Errorf("bad addr: %s", addr)
	}
	group := parts[0]
	port := parts[1]

	// resolve group/port (not used directly; we bind to :port)

	var ifi *net.Interface
	if ifname != "" {
		ifi, err := net.InterfaceByName(ifname)
		if err != nil {
			return nil, err
		}
		_ = ifi
	} else {
		ifaces, err := net.Interfaces()
		if err != nil {
			return nil, err
		}
		for _, i := range ifaces {
			if (i.Flags&net.FlagUp) != 0 && (i.Flags&net.FlagMulticast) != 0 && (i.Flags&net.FlagLoopback) == 0 {
				ifi = &i
				break
			}
		}
	}

	// Create a socket with SO_REUSEADDR and SO_REUSEPORT where available, before binding.
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var ctrlErr error
			if err := c.Control(func(fd uintptr) {
				// set SO_REUSEADDR
				if e := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); e != nil {
					ctrlErr = e
					return
				}
				// try SO_REUSEPORT on non-Windows platforms
				if runtime.GOOS != "windows" {
					if e := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEPORT, 1); e != nil {
						// non-fatal; record but continue
						ctrlErr = e
					}
				}
			}); err != nil {
				return err
			}
			return ctrlErr
		},
	}

	pcConn, err := lc.ListenPacket(context.Background(), "udp4", ":"+port)
	if err != nil {
		return nil, err
	}
	c, ok := pcConn.(*net.UDPConn)
	if !ok {
		pcConn.Close()
		return nil, fmt.Errorf("unexpected PacketConn type")
	}
	_ = c.SetReadBuffer(4 * 1024 * 1024)

	// Try to join multicast group on the socket so we receive group datagrams.
	pconn := ipv4.NewPacketConn(c)
	// enable loopback to allow receiving multicast sent from this host
	_ = pconn.SetMulticastLoopback(true)
	joined := false
	mip := net.ParseIP(group)
	if ifi != nil {
		if err := pconn.JoinGroup(ifi, &net.UDPAddr{IP: mip}); err == nil {
			joined = true
			log.Printf("joined multicast group %s on iface %s", group, ifi.Name)
		} else {
			log.Printf("warning: failed to join multicast group %s on iface %s: %v", group, ifi.Name, err)
		}
	} else {
		ifaces, _ := net.Interfaces()
		for _, ii := range ifaces {
			if (ii.Flags&net.FlagUp) != 0 && (ii.Flags&net.FlagMulticast) != 0 && (ii.Flags&net.FlagLoopback) == 0 {
				if err := pconn.JoinGroup(&ii, &net.UDPAddr{IP: mip}); err == nil {
					joined = true
					log.Printf("joined multicast group %s on iface %s", group, ii.Name)
					break
				} else {
					log.Printf("warning: failed to join multicast group %s on iface %s: %v", group, ii.Name, err)
				}
			}
		}
	}
	if !joined {
		log.Printf("warning: could not join multicast group %s on any interface; continuing to listen on :%s", group, port)
	}

	r := &Receiver{conn: c, buf: make([]byte, 65536), frames: make(map[uint32]*assemblingFrame), out: make(chan []byte, 8), stop: make(chan struct{})}

	go r.readLoop()
	go r.purgeLoop()

	return r, nil
}

func (r *Receiver) readLoop() {
	for {
		select {
		case <-r.stop:
			return
		default:
		}
		n, addr, err := r.conn.ReadFromUDP(r.buf)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		// debug log each received UDP packet
		log.Printf("recv UDP %d bytes from %v", n, addr)
		// debug
		_ = addr
		if n < fragHeaderSize {
			// legacy or small packet: treat as whole payload
			b := make([]byte, n)
			copy(b, r.buf[:n])
			select {
			case r.out <- b:
			default:
			}
			continue
		}
		if r.buf[0] != fragVersion {
			// not our frag format; ignore or treat as legacy
			b := make([]byte, n)
			copy(b, r.buf[:n])
			select {
			case r.out <- b:
			default:
			}
			continue
		}
		frameID := binary.BigEndian.Uint32(r.buf[1:5])
		total := binary.BigEndian.Uint16(r.buf[5:7])
		idx := binary.BigEndian.Uint16(r.buf[7:9])
		payload := make([]byte, n-fragHeaderSize)
		copy(payload, r.buf[fragHeaderSize:n])

		r.mu.Lock()
		af, ok := r.frames[frameID]
		if !ok {
			af = &assemblingFrame{total: total, parts: make(map[uint16][]byte), created: time.Now()}
			r.frames[frameID] = af
		}
		if _, exists := af.parts[idx]; !exists {
			af.parts[idx] = payload
			af.received++
		}
		if af.received == int(af.total) {
			// assemble
			var full []byte
			for i := uint16(0); i < af.total; i++ {
				part := af.parts[i]
				full = append(full, part...)
			}
			delete(r.frames, frameID)
			r.mu.Unlock()
			select {
			case r.out <- full:
			default:
			}
			continue
		}
		r.mu.Unlock()
	}
}

func (r *Receiver) purgeLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-5 * time.Second)
			r.mu.Lock()
			for id, af := range r.frames {
				if af.created.Before(cutoff) {
					delete(r.frames, id)
				}
			}
			r.mu.Unlock()
		}
	}
}

// Next returns the next fully reassembled frame (blocks). It will return
// legacy small packets as-is and assembled fragments when available.
func (r *Receiver) Next() ([]byte, error) {
	b, ok := <-r.out
	if !ok {
		return nil, fmt.Errorf("receiver closed")
	}
	return b, nil
}

func (r *Receiver) Close() error {
	close(r.stop)
	close(r.out)
	return r.conn.Close()
}
