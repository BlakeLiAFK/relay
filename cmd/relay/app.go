package main

import (
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/DGHeroin/relay"
)

var (
	local  = flag.String("l", ":53", "Listen Local Address")
	remote = flag.String("r", "1.1.1.1:53", "Forward To Remote Address")
	u      = flag.Bool("u", true, "Forward UDP")
	config = flag.String("c", "", "config file")
)

type Config struct {
	Configs []LinkConfig
}

type LinkConfig struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
	U   bool   `json:"udp"`
}

func OpenRelay(src, dst string, enableUDP bool) func() {
	stopCh := make(chan struct{})

	tcp := relay.NewTCPRelay()
	go tcp.Serve(src, dst, stopCh)

	// Fix: Use function parameter instead of global flag
	if enableUDP {
		udp := relay.NewUDPRelay()
		go udp.Serve(src, dst, stopCh)
	}

	return func() {
		close(stopCh)
	}
}

func ServeConfig(cfg *Config) []func() {
	cleanups := make([]func(), 0, len(cfg.Configs))
	for _, c := range cfg.Configs {
		cleanup := OpenRelay(c.Src, c.Dst, c.U)
		cleanups = append(cleanups, cleanup)
	}
	return cleanups
}

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// Fix: Store cleanup functions
	var cleanups []func()

	if *config != "" {
		data, err := os.ReadFile(*config)
		if err != nil {
			log.Println("Failed to read config:", err)
			return
		}

		cfg := &Config{}
		if err := json.Unmarshal(data, cfg); err != nil {
			log.Println("Failed to parse config:", err)
			return
		}

		cleanups = ServeConfig(cfg)
		log.Printf("Started %d relay(s) from config", len(cleanups))
	} else {
		cleanup := OpenRelay(*local, *remote, *u)
		cleanups = append(cleanups, cleanup)
	}

	stopSig := make(chan os.Signal, 1)
	signal.Notify(stopSig, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
	sig := <-stopSig
	log.Println("Received signal:", sig)

	// Fix: Call cleanup functions for graceful shutdown
	log.Println("Shutting down relays gracefully...")
	for i, cleanup := range cleanups {
		log.Printf("Stopping relay %d/%d", i+1, len(cleanups))
		cleanup()
	}

	// Give some time for goroutines to finish
	time.Sleep(500 * time.Millisecond)
	log.Println("Shutdown complete")
}
