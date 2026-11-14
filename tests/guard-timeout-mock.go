// SPDX-License-Identifier: GPL-3.0-or-later
// The independent mock deliberately takes longer than upstream mosdns's
// original 5-second entry deadline to validate the full DNS response chain.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	http.HandleFunc("/confirm", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(6 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"pin_id":"0123456789abcdef01234567"}`)
	})
	http.HandleFunc("/guardpin/may-send", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	http.HandleFunc("/guardpin/finish-sent", func(w http.ResponseWriter, r *http.Request) {
		f, err := os.OpenFile("/test/finish", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprintln(f, "finished")
		_ = f.Close()
		w.WriteHeader(http.StatusNoContent)
	})
	if err := http.ListenAndServe("127.0.0.1:18080", nil); err != nil {
		panic(err)
	}
}
