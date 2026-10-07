// Command fakellm serves the fake model and alert sink of VibeCI's
// end-to-end tests (used by the compose test's container).
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/vibeci/vibeci/e2e/fakellm"
)

func main() {
	listen := flag.String("listen", ":8080", "listen address")
	script := flag.String("script", "", "script JSON file")
	alerts := flag.String("alerts", "", "append received alerts to this file (JSON lines)")
	flag.Parse()
	s := &fakellm.Server{AlertsFile: *alerts}
	if *script != "" {
		sc, err := fakellm.LoadScript(*script)
		if err != nil {
			log.Fatal(err)
		}
		s.Script = sc
	}
	srv := &http.Server{Addr: *listen, Handler: s, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("fakellm listening on %s", *listen)
	log.Fatal(srv.ListenAndServe())
}
