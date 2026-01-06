# codebits-tv

![codebits-tv](docs/icon.png)

This is a completely off the wall experiment in streaming generated video frames over multicast UDP, heavily inspired by three generations of digital signage solutions I hacked together for Codebits/PixelsCamp events over the years.

> Hat tip to [@chbm](https://github.com/chbm) for the name

Stay tuned for the ESP32 version!

## Tools:

- `server`: generates 5 FPS JPEG frames and multicasts them on the LAN.
- `proxy`: joins the multicast group and exposes an MJPEG HTTP endpoint and a small viewer at `/`.
- `cli`: opens the proxy MJPEG URL in the system browser.

Build:

```bash
go build ./cmd/...
```

Run examples (in separate terminals):

```bash
# server: basic multicast sender
./bin/server -addr 224.0.0.250:5000

# proxy: join multicast and serve MJPEG at http://localhost:8080/stream
./bin/proxy -addr 224.0.0.250:5000 -http :8080
# if the proxy cannot join the multicast group automatically, specify the interface name
./proxy -addr 224.0.0.250:5000 -http :8080 -if en0
./cli -url http://localhost:8080/stream

# Slideshow with crossfade and JPEG quality
./bin/server -slides "/path/to/slides" -slide-interval 5 -fade 2 -quality 70
```

## Notes:

- The server encodes frames at ~5 FPS using JPEG with a timestamp overlay.
- The multicast framing uses a 4-byte length prefix when possible; the proxy understands this framing.

- If the proxy logs warnings about joining the multicast group, specify the correct interface with `-if`.
- On macOS use `ifconfig` to find candidate interfaces (e.g. `en0`); on Linux use `ip link`.

- The proxy also serves a small HTML viewer at `/` that embeds the MJPEG stream.
- Both server and proxy support graceful shutdown on SIGINT (Ctrl+C).

Performance & Notes:

- Crossfade (`-fade`): when enabled the server will blend the last `F` seconds of each slide transition. Blending is done per-pixel on full 1920×1080 RGBA frames and is parallelized across CPU cores. This produces smooth crossfades but increases CPU usage during transitions.
- JPEG quality (`-quality`): controls the JPEG encoder quality (1-100). Lower values reduce bandwidth at the cost of visual fidelity and may speed up encoding.
- Proxy viewer: the HTML viewer at `/` scales the MJPEG image to fill the browser viewport while preserving aspect ratio (no stretching). The image will be letterboxed/pillarboxed as needed.
- Tuning: if CPU is a concern, reduce `-fade`, reduce the `-quality`, or lower the output resolution in `internal/frame`.

- Send behavior: by default the server only multicasts when the encoded frame bytes differ from the previous one (this includes frames produced by fades). There is no `-send-on-change` flag — this is the default behavior.
- Timestamp overlay: the timestamp is off by default. Enable it with the `-timestamp` flag when starting the server.

If you'd like runtime control over `fade`/`quality` (HTTP admin endpoint) or further optimizations (buffer reuse, configurable worker count), I can add those next.
