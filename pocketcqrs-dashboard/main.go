// Command pocketcqrs-dashboard is the CQRS ops dashboard: a separate,
// self-contained binary that consumes a pocketcqrs instance over its public
// API only (never internals, never the event store). See docs/consuming.md
// (DASH.4) for deployment patterns.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
)

func main() {
	backend := flag.String("backend", "http://127.0.0.1:8090", "URL of the pocketcqrs backend")
	listen := flag.String("listen", "127.0.0.1:8091", "address the dashboard listens on")
	version := flag.Bool("version", false, "print the module version and exit")
	flag.Parse()

	if *version {
		v := "(devel)"
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
			v = bi.Main.Version
		}
		fmt.Println("pocketcqrs-dashboard", v)
		return
	}

	s := newServer(*backend)
	log.Printf("pocketcqrs-dashboard: listening on http://%s (backend %s)", *listen, *backend)
	log.Fatal(http.ListenAndServe(*listen, s.routes()))
}
