package main

import (
	"log"

	"github.com/jagadeesh/grainlify/backend/internal/config"
)

// Worker entrypoint placeholder.
//
// Reads config.ExternalWorkerImplemented rather than restating the fact, so
// this binary and the refusal in cmd/api cannot drift into disagreeing about
// whether an external worker exists. When this does something, flip the
// constant there and cmd/api stops refusing NATS_URL.
func main() {
	if !config.ExternalWorkerImplemented {
		log.Println("worker is not implemented in this build; cmd/api runs the background jobs in-process when NATS_URL is unset")
		return
	}
	log.Println("worker starting")
}
