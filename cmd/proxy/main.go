package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"time"

	"mjpeg-multicast/internal/mcast"
)

type client struct {
	ch chan []byte
}

type hub struct {
	mu      sync.Mutex
	clients map[*client]struct{}
}

var broadcasted uint64

func newHub() *hub { return &hub{clients: make(map[*client]struct{})} }

func (h *hub) add(c *client)    { h.mu.Lock(); h.clients[c] = struct{}{}; h.mu.Unlock() }
func (h *hub) remove(c *client) { h.mu.Lock(); delete(h.clients, c); close(c.ch); h.mu.Unlock() }
func (h *hub) broadcast(frame []byte) {
	h.mu.Lock()
	for c := range h.clients {
		select {
		case c.ch <- frame:
		default:
			// slow client, drop
		}
	}
	h.mu.Unlock()
}

func main() {
	addr := flag.String("addr", "224.0.0.250:5000", "multicast address:port")
	httpAddr := flag.String("http", ":8080", "http listen address")
	ifname := flag.String("if", "", "network interface name to use for multicast (optional)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExample:\n  %s -addr 224.0.0.250:5000 -http :8080\n", os.Args[0])
	}
	flag.Parse()

	rx, err := mcast.NewReceiver(*addr, *ifname)
	if err != nil {
		log.Fatalf("receiver: %v", err)
	}
	defer rx.Close()

	h := newHub()

	// background reader
	go func() {
		for {
			img, err := rx.Next()
			if err != nil {
				log.Printf("rx: %v", err)
				time.Sleep(500 * time.Millisecond)
				continue
			}
			h.broadcast(img)
			cnt := atomic.AddUint64(&broadcasted, 1)
			if cnt%10 == 0 {
				log.Printf("broadcasted frames: %d", cnt)
			}
		}
	}()

	// periodic stats
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			h.mu.Lock()
			clients := len(h.clients)
			h.mu.Unlock()
			log.Printf("hub: clients=%d", clients)
		}
	}()

	http.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")

		c := &client{ch: make(chan []byte, 2)}
		h.add(c)
		defer h.remove(c)

		// send frames to client until disconnect
		for {
			select {
			case f := <-c.ch:
				if _, err := fmt.Fprintf(w, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(f)); err != nil {
					return
				}
				if _, err := w.Write(f); err != nil {
					return
				}
				if _, err := fmt.Fprint(w, "\r\n"); err != nil {
					return
				}
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!doctype html>
<html>
<head>
	<meta name="viewport" content="width=device-width,initial-scale=1" />
	<style>
		html,body{height:100%;margin:0;background:#000}
		.frame{display:flex;align-items:center;justify-content:center;height:100%;}
		.frame img{max-width:100%;max-height:100%;width:auto;height:auto;object-fit:contain}
	</style>
</head>
<body>
	<div class="frame"><img src="/stream" alt="MJPEG stream"/></div>
</body>
</html>`)
	})

	srv := &http.Server{Addr: *httpAddr}
	go func() {
		log.Printf("http listening %s", *httpAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ListenAndServe: %v", err)
		}
	}()

	// wait for interrupt and gracefully shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	log.Printf("shutting down http server")
	_ = srv.Shutdown(context.Background())
}
