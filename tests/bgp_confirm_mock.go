// SPDX-License-Identifier: GPL-3.0-or-later
// Mock confirmation service used by bgp-integration.sh.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	http.HandleFunc("/confirm", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 8192))
		f, e := os.OpenFile("/data/requests", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if e != nil {
			w.WriteHeader(500)
			return
		}
		fmt.Fprintln(f, string(b))
		f.Close()
		if _, e = os.Stat("/data/fail"); e == nil {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(204)
	})
	if e := http.ListenAndServe(":8080", nil); e != nil {
		panic(e)
	}
}
