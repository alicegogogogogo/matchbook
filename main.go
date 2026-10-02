// Command matchbook runs the Matchbook limit order book matching engine and its
// deterministic replay API.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"matchbook/internal/engine"
	"matchbook/internal/server"
)

const productName = "Matchbook"

func main() {
	host := flag.String("host", "127.0.0.1", "interface to bind")
	port := flag.Int("port", 8080, "TCP port to bind")
	dataDir := flag.String("data", "data", "directory holding the event log")
	symbols := flag.String("symbols", "", "comma separated symbol list registered at startup as SYMBOL:SCALE")
	flag.Parse()

	matching, err := engine.NewEngine(*dataDir, func() int64 {
		return time.Now().UnixMilli()
	})
	if err != nil {
		log.Fatalf("matchbook: %v", err)
	}
	defer matching.Close()

	if err := seedSymbols(matching, *symbols); err != nil {
		log.Fatalf("matchbook: %v", err)
	}

	listener, err := server.Listen(*host, *port)
	if err != nil {
		log.Fatalf("matchbook: %v", err)
	}
	fmt.Printf("%s listening on http://%s:%d\n", productName, *host, *port)

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	done := make(chan error, 1)
	go func() {
		done <- server.Serve(listener, server.New(matching).Handler())
	}()
	select {
	case err := <-done:
		if err != nil {
			log.Fatalf("matchbook: %v", err)
		}
	case <-shutdown:
		if err := listener.Close(); err != nil {
			log.Printf("matchbook: %v", err)
		}
	}
}

// seedSymbols registers the symbols requested with --symbols, skipping ones that
// are already in the event log so restarts stay idempotent.
func seedSymbols(matching *engine.Engine, specification string) error {
	for _, entry := range strings.Split(specification, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, scale, err := parseSymbolSpec(entry)
		if err != nil {
			return err
		}
		if _, err := matching.AddSymbol(name, scale); err != nil {
			if !isDuplicate(err) {
				return err
			}
		}
	}
	return nil
}

func parseSymbolSpec(entry string) (string, int64, error) {
	parts := strings.Split(entry, ":")
	if len(parts) > 2 {
		return "", 0, fmt.Errorf("symbol specification %q must be SYMBOL or SYMBOL:SCALE", entry)
	}
	scale := int64(0)
	if len(parts) == 2 {
		parsed, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || parsed < 0 || parsed > 8 {
			return "", 0, fmt.Errorf("symbol specification %q must use a price scale between 0 and 8", entry)
		}
		scale = int64(parsed)
	}
	return strings.TrimSpace(parts[0]), scale, nil
}

func isDuplicate(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already exists")
}
