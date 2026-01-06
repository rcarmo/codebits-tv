package mcast

import (
	"encoding/binary"
	"testing"
	"time"
)

func TestFragmentHeaderAndAssemble(t *testing.T) {
	// create a fake payload and fragment it the same way as SendFrame
	payload := make([]byte, 5000)
	for i := range payload {
		payload[i] = byte(i & 0xff)
	}

	mtu := 1200
	payloadPer := mtu - fragHeaderSize
	if payloadPer <= 0 {
		t.Fatalf("bad payloadPer")
	}
	total := (len(payload) + payloadPer - 1) / payloadPer

	// emulate receiver
	r := &Receiver{frames: make(map[uint32]*assemblingFrame), out: make(chan []byte, 4)}
	frameID := uint32(42)

	for i := 0; i < total; i++ {
		start := i * payloadPer
		end := start + payloadPer
		if end > len(payload) {
			end = len(payload)
		}
		frag := make([]byte, fragHeaderSize+(end-start))
		frag[0] = fragVersion
		binary.BigEndian.PutUint32(frag[1:5], frameID)
		binary.BigEndian.PutUint16(frag[5:7], uint16(total))
		binary.BigEndian.PutUint16(frag[7:9], uint16(i))
		copy(frag[fragHeaderSize:], payload[start:end])

		// feed fragment processing logic (simulating readLoop body)
		frameID2 := binary.BigEndian.Uint32(frag[1:5])
		total2 := binary.BigEndian.Uint16(frag[5:7])
		idx := binary.BigEndian.Uint16(frag[7:9])
		payloadPart := make([]byte, len(frag)-fragHeaderSize)
		copy(payloadPart, frag[fragHeaderSize:])

		af, ok := r.frames[frameID2]
		if !ok {
			af = &assemblingFrame{total: total2, parts: make(map[uint16][]byte), created: time.Now()}
			r.frames[frameID2] = af
		}
		if _, exists := af.parts[idx]; !exists {
			af.parts[idx] = payloadPart
			af.received++
		}
		if af.received == int(af.total) {
			var full []byte
			for i := uint16(0); i < af.total; i++ {
				full = append(full, af.parts[i]...)
			}
			if len(full) != len(payload) {
				t.Fatalf("assembled size mismatch: %d vs %d", len(full), len(payload))
			}
			// verify content
			for i := range payload {
				if payload[i] != full[i] {
					t.Fatalf("mismatch at %d", i)
				}
			}
			return
		}
	}

	t.Fatalf("did not assemble frame")
}
