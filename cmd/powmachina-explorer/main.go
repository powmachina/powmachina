// powmachina-explorer is the PowMachina blockchain explorer.
//
// A separate process. It reads state from the node over its native JSON-RPC and keeps no
// copy of the chain — no database, no indexer, no state that could diverge. The indexes it
// stands on (transactions and addresses) are kept by the node, which writes them in the
// same atomic batch as the block.
//
// The node has to run with `-txindex -addrindex`, otherwise the explorer shows blocks but
// neither transactions by txid nor address history.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"powmachina/internal/explorer"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "address to listen on")
	nodeURL := flag.String("node", "http://127.0.0.1:7697", "RPC address of the node")
	tokenFile := flag.String("token-file", "", "file with the node RPC token")
	base := flag.String("base", "/explorer", "prefix the explorer is served under")
	network := flag.String("network", "", "network name for the header (empty = ask the node)")
	timeout := flag.Duration("timeout", 10*time.Second, "limit for a single request")
	geoNodes := flag.String("geo-nodes", "", "more node RPC URLs whose share countries the SharePool map adds, comma separated (token: POWM_GEO_RPC_TOKEN)")
	flag.Parse()

	// The token comes from a file or from the environment, never from an argument: arguments
	// are visible to anybody who runs `ps`.
	token := os.Getenv("POWM_RPC_TOKEN")
	if *tokenFile != "" {
		raw, err := os.ReadFile(*tokenFile)
		if err != nil {
			log.Fatalf("token: %v", err)
		}
		token = strings.TrimSpace(string(raw))
	}

	cli := explorer.NewClient(*nodeURL, token, *timeout)
	var geo []*explorer.Client
	for _, u := range strings.Split(*geoNodes, ",") {
		if u = strings.TrimSpace(u); u != "" {
			geo = append(geo, explorer.NewClient(u, os.Getenv("POWM_GEO_RPC_TOKEN"), 3*time.Second))
		}
	}
	srv, err := explorer.NewServer(cli, explorer.Config{
		BasePath: *base,
		Network:  *network,
		Timeout:  *timeout,
		GeoNodes: geo,
	})
	if err != nil {
		log.Fatalf("explorer: %v", err)
	}

	h := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	fmt.Printf("explorer listening on %s%s/ (node %s)\n", *listen, *base, *nodeURL)
	if err := h.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
