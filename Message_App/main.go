package main

import (
	"flag"
	"log"
	"net/http"
	"os"
)

func main() {
	defaultPort := os.Getenv("PORT")
	if defaultPort == "" {
		defaultPort = "8080"
	}
	address := flag.String("addr", ":"+defaultPort, "HTTP listen address")
	flag.Parse()

	server := newServer("index.html", os.Getenv("ALLOWED_ORIGINS"))
	defer server.close()
	log.Printf("Relay listening on %s", *address)
	if err := http.ListenAndServe(*address, server.routes()); err != nil {
		log.Fatal(err)
	}
}
