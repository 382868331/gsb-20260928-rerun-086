package main

import (
	"flag"
	"log"
	"net/http"

	"gameplatform/internal/platform"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dataDir := flag.String("data", "./data", "journal directory")
	flag.Parse()

	srv := platform.NewServer(*dataDir)
	log.Printf("authoritative game server listening on %s (journals in %s)", *addr, *dataDir)
	if err := http.ListenAndServe(*addr, srv.Routes()); err != nil {
		log.Fatal(err)
	}
}
