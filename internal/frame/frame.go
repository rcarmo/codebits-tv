package frame

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	_ "golang.org/x/image/bmp"
	draw2 "golang.org/x/image/draw"
	xfont "golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

var (
	// default geometry; can be changed via SetGeometry
	frameW = 1920
	frameH = 1080

	mu            sync.RWMutex
	slides        []image.Image
	cur           int
	lastAdvance   time.Time
	interval      = 1 * time.Second
	fadeDuration  = 0 * time.Second
	quality       = 80
	showTimestamp = false
)

// SetGeometry sets the output frame width and height (in pixels).
func SetGeometry(w, h int) {
	if w <= 0 || h <= 0 {
		return
	}
	mu.Lock()
	frameW = w
	frameH = h
	mu.Unlock()
}

// StartSlideshow loads images from dir and begins cycling them every dt.
func StartSlideshow(dir string, dt time.Duration) error {
	imgs, err := loadImages(dir)
	if err != nil {
		return err
	}
	if len(imgs) == 0 {
		return errors.New("no images found")
	}

	mu.Lock()
	slides = imgs
	cur = 0
	lastAdvance = time.Now()
	interval = dt
	mu.Unlock()
	return nil
}

// SetFade sets a crossfade duration between slides. A zero duration disables fading.
func SetFade(d time.Duration) {
	mu.Lock()
	fadeDuration = d
	mu.Unlock()
}

// SetQuality sets the JPEG encoding quality (1-100)
func SetQuality(q int) {
	if q < 1 {
		q = 1
	}
	if q > 100 {
		q = 100
	}
	mu.Lock()
	quality = q
	mu.Unlock()
}

// SetTimestamp enables or disables drawing the timestamp overlay.
func SetTimestamp(enabled bool) {
	mu.Lock()
	showTimestamp = enabled
	mu.Unlock()
}

// loadImages finds supported image files in the directory and decodes them.
func loadImages(dir string) ([]image.Image, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(p)
		switch ext {
		case ".jpg", ".jpeg", ".png", ".gif", ".bmp":
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var imgs []image.Image
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		img, _, err := image.Decode(f)
		f.Close()
		if err != nil {
			continue
		}
		// scale / center to configured geometry
		mu.RLock()
		fw, fh := frameW, frameH
		mu.RUnlock()
		dst := image.NewRGBA(image.Rect(0, 0, fw, fh))
		draw2.Draw(dst, dst.Bounds(), &image.Uniform{C: color.Black}, image.Point{}, draw2.Src)
		// fit preserving aspect
		w := img.Bounds().Dx()
		h := img.Bounds().Dy()
		rw := float64(fw) / float64(w)
		rh := float64(fh) / float64(h)
		scale := rw
		if rh < rw {
			scale = rh
		}
		nw := int(float64(w) * scale)
		nh := int(float64(h) * scale)
		// center
		offX := (fw - nw) / 2
		offY := (fh - nh) / 2
		tmp := image.NewRGBA(image.Rect(0, 0, nw, nh))
		draw2.ApproxBiLinear.Scale(tmp, tmp.Bounds(), img, img.Bounds(), draw2.Over, nil)
		draw.Draw(dst, image.Rect(offX, offY, offX+nw, offY+nh), tmp, image.Point{}, draw.Src)
		imgs = append(imgs, dst)
	}
	return imgs, nil
}

// GenerateFrame returns the current slide as a JPEG, advancing if interval elapsed.
func GenerateFrame() ([]byte, error) {
	mu.Lock()
	fw, fh := frameW, frameH
	if len(slides) == 0 {
		mu.Unlock()
		// fallback: generate a simple timestamp image
		dst := image.NewRGBA(image.Rect(0, 0, fw, fh))
		draw2.Draw(dst, dst.Bounds(), &image.Uniform{C: color.Black}, image.Point{}, draw2.Src)
		addLabel(dst, 20, fh-30, time.Now().Format("2006-01-02 15:04:05"))
		var buf bytes.Buffer
		mu.RLock()
		q := quality
		mu.RUnlock()
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: q}); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	now := time.Now()
	elapsed := now.Sub(lastAdvance)
	var img image.Image
	// determine if we should advance slide or produce a blended frame
	if elapsed >= interval {
		cur = (cur + 1) % len(slides)
		lastAdvance = now
		img = slides[cur]
		mu.Unlock()
	} else if fadeDuration > 0 && elapsed >= interval-fadeDuration {
		// produce blended image between cur and next
		next := (cur + 1) % len(slides)
		// copy references while holding lock then release
		a := slides[cur].(*image.RGBA)
		b := slides[next].(*image.RGBA)
		mu.Unlock()
		// compute alpha in [0,1]
		alpha := float64(elapsed-(interval-fadeDuration)) / float64(fadeDuration)
		if alpha < 0 {
			alpha = 0
		}
		if alpha > 1 {
			alpha = 1
		}
		// blend per-pixel in parallel by rows
		rgba := image.NewRGBA(image.Rect(0, 0, fw, fh))
		apix := a.Pix
		bpix := b.Pix
		dpix := rgba.Pix
		stride := rgba.Stride
		// decide workers
		workers := 4
		if n := runtime.NumCPU(); n > workers {
			workers = n
		}
		var wg sync.WaitGroup
		rowsPer := fh / workers
		for w := 0; w < workers; w++ {
			startRow := w * rowsPer
			endRow := startRow + rowsPer
			if w == workers-1 {
				endRow = fh
			}
			wg.Add(1)
			go func(sr, er int) {
				defer wg.Done()
				for y := sr; y < er; y++ {
					rowStart := y * stride
					for x := 0; x < fw; x++ {
						i := rowStart + x*4
						ar := float64(apix[i])
						ag := float64(apix[i+1])
						ab := float64(apix[i+2])
						aa := float64(apix[i+3])
						br := float64(bpix[i])
						bg := float64(bpix[i+1])
						bb := float64(bpix[i+2])
						ba := float64(bpix[i+3])
						dpix[i] = uint8((1-alpha)*ar + alpha*br)
						dpix[i+1] = uint8((1-alpha)*ag + alpha*bg)
						dpix[i+2] = uint8((1-alpha)*ab + alpha*bb)
						dpix[i+3] = uint8((1-alpha)*aa + alpha*ba)
					}
				}
			}(startRow, endRow)
		}
		wg.Wait()
		img = rgba
	} else {
		img = slides[cur]
		mu.Unlock()
	}

	// overlay timestamp (optional)
	rgba := image.NewRGBA(image.Rect(0, 0, fw, fh))
	draw.Draw(rgba, rgba.Bounds(), img, image.Point{}, draw.Src)
	mu.RLock()
	ts := showTimestamp
	mu.RUnlock()
	if ts {
		addLabel(rgba, 20, fh-30, time.Now().Format("2006-01-02 15:04:05"))
	}

	var buf bytes.Buffer
	mu.RLock()
	q := quality
	mu.RUnlock()
	if err := jpeg.Encode(&buf, rgba, &jpeg.Options{Quality: q}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func addLabel(img *image.RGBA, x, y int, label string) {
	col := color.RGBA{255, 255, 255, 255}
	face := basicfont.Face7x13
	d := &xfont.Drawer{
		Dst:  img,
		Src:  image.NewUniform(col),
		Face: face,
		Dot:  fixed.Point26_6{X: fixed.I(x), Y: fixed.I(y)},
	}
	d.DrawString(label)
}
