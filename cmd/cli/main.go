package main

import (
	"flag"
	"log"
	"os/exec"
	"runtime"
)

func main() {
	url := flag.String("url", "http://localhost:8080/stream", "proxy stream URL")
	flag.Parse()

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", *url)
	case "linux":
		cmd = exec.Command("xdg-open", *url)
	default:
		log.Fatalf("unsupported OS: %s", runtime.GOOS)
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("open: %v", err)
	}
}
