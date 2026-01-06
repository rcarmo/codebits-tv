package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"log"
	"math"
	"mjpeg-multicast/internal/frame"
	"mjpeg-multicast/internal/mcast"
	"os"
	"os/signal"
	"time"
)

var lastHash [32]byte

func main() {
	addr := flag.String("addr", "224.0.0.250:5000", "multicast address:port")
	ifname := flag.String("if", "", "network interface name to use for multicast (optional)")
	ttl := flag.Int("ttl", 1, "multicast TTL (1=local LAN)")
	mtu := flag.Int("mtu", 1200, "MTU to fragment UDP packets to")
	repeats := flag.Int("repeats", 1, "how many times to repeat each fragment for redundancy")
	slides := flag.String("slides", "", "directory containing images to use as slideshow")
	slideInterval := flag.Int("slide-interval", 5, "slideshow interval in seconds")
	fade := flag.Int("fade", 0, "crossfade duration in seconds (0 to disable)")
	quality := flag.Int("quality", 80, "JPEG encoding quality (1-100)")
	geometry := flag.String("geometry", "1920x1080", "output frame geometry WIDTHxHEIGHT, e.g. 1280x720")
	timestamp := flag.Bool("timestamp", false, "enable timestamp overlay on frames")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExample:\n  %s -slides \"/path/to/slides\" -slide-interval 5 -fade 2 -quality 70 -geometry 1280x720\n", os.Args[0])
	}
	flag.Parse()

	// parse geometry WIDTHxHEIGHT
	var gw, gh int
	if _, err := fmt.Sscanf(*geometry, "%dx%d", &gw, &gh); err == nil {
		if gw > 0 && gh > 0 {
			frame.SetGeometry(gw, gh)
		}
	}

	if *slides != "" {
		if err := frame.StartSlideshow(*slides, time.Duration(*slideInterval)*time.Second); err != nil {
			log.Fatalf("StartSlideshow: %v", err)
		}
		if *fade > 0 {
			frame.SetFade(time.Duration(*fade) * time.Second)
		}
		if *quality != 80 {
			frame.SetQuality(*quality)
		}
		// timestamp overlay is opt-in; default is off
		if *timestamp {
			frame.SetTimestamp(true)
		}
	}

	sender, err := mcast.NewSender(*addr, *ifname, *ttl)
	if err != nil {
		log.Fatalf("sender: %v", err)
	}
	defer sender.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	ticker := time.NewTicker(time.Second / 5)
	defer ticker.Stop()
	sent := 0
	var lastSendTime time.Time
	var ewmaBps float64
	// EWMA time constant in seconds (5s)
	const tau = 5.0
	for {
		select {
		case <-ctx.Done():
			log.Printf("shutting down server")
			return
		case <-ticker.C:
			img, err := frame.GenerateFrame()
			if err != nil {
				log.Printf("frame: %v", err)
				continue
			}
			// default behavior: only send when encoded bytes change
			h := sha256.Sum256(img)
			if bytes.Equal(h[:], lastHash[:]) {
				// same frame, skip sending
				continue
			}
			lastHash = h
			if err := sender.SendFrame(img, *mtu, *repeats); err != nil {
				log.Printf("send: %v", err)
			} else {
				// estimate bandwidth for this frame on-wire
				// fragment header size matches internal/mcast fragHeaderSize (1+4+2+2=9)
				const fragHeader = 9
				const ipUdpOverhead = 28
				mtuVal := *mtu
				payloadPer := mtuVal - fragHeader
				if payloadPer <= 0 {
					payloadPer = 1191
				}
				payloadLen := len(img)
				fragments := (payloadLen + payloadPer - 1) / payloadPer
				bytesOnWire := payloadLen + fragments*(fragHeader+ipUdpOverhead)
				bytesWithRepeats := bytesOnWire * (*repeats)
				// fps is the ticker frequency (5Hz); we compute instant bps from actual send interval below
				// compute instant bps using delta time since last send
				now := time.Now()
				var instBps float64
				if !lastSendTime.IsZero() {
					dt := now.Sub(lastSendTime).Seconds()
					if dt > 0 {
						instBps = float64(bytesWithRepeats) * 8.0 / dt
					}
				}
				lastSendTime = now
				// update EWMA: alpha = 1 - exp(-dt/tau)
				var alpha float64 = 0.0
				if ewmaBps == 0 {
					ewmaBps = instBps
				} else {
					// use dt from 1/fps if instBps==0 (shouldn't happen)
					dt := 1.0 / 5.0
					alpha = 1 - math.Exp(-dt/tau)
					ewmaBps = alpha*instBps + (1-alpha)*ewmaBps
				}
				log.Printf("frame: bytes=%d fragments=%d bytes_on_wire=%d repeats=%d inst=%.3f Mbps ewma=%.3f Mbps", payloadLen, fragments, bytesWithRepeats, *repeats, instBps/1e6, ewmaBps/1e6)
			}
			sent++
			if sent%10 == 0 {
				log.Printf("sent frames: %d", sent)
			}
		}
	}
}
