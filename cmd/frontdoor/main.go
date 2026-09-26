package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
)

func main() {
	addr := flag.String("listen-address", ":8080", "Proxy listen address.")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not implemented", http.StatusNotImplemented)
	})

	slog.Info("starting frontdoor", "addr", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		slog.Error("frontdoor stopped", "err", err)
		os.Exit(1)
	}
}
