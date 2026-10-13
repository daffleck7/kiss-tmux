package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"runtime"
)

func main() {
	port := flag.Int("port", 7777, "Port to listen on")
	open := flag.Bool("open", true, "Auto-open browser")
	flag.Parse()

	cfg := loadConfig()

	hub := NewHub()
	go hub.Run()

	mgr := NewManager(8, func(data []byte) {
		hub.broadcast <- data
	})
	mgr.SetHosts(cfg.Hosts)

	handler := SetupHTTP(hub, mgr, cfg, *port)

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	url := fmt.Sprintf("http://%s", addr)

	// Listen before discovery: a second instance must die here, before it attaches
	// to the Mac's sessions.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	go mgr.DiscoverAll()

	log.Printf("KISS-TMUX serving at %s", url)

	if *open {
		go openBrowser(url)
	}

	if err := http.Serve(listener, handler); err != nil {
		log.Fatal(err)
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Start()
}
