// main.go — the forge entrypoint (Go binary).
//
// Lifecycle (from the competition rules):
//  1. bind an ephemeral port on 127.0.0.1 (never 8080 — that port belongs to
//     the env),
//  2. POST {"port": <port>} to $MAKEFUN_ENV_URL/register (the first
//     registration wins; a later one is answered 409),
//  3. poll GET $MAKEFUN_ENV_URL/done with exponential backoff
//     (250 ms -> 2.000 ms) until it answers {"done": true}, then exit 0.
// Without MAKEFUN_ENV_URL the forge runs standalone on its ephemeral port
// and keeps serving (handy for local runs against the kit's sdk/run.ts).
//
// The repository's node entrypoint (main.ts) is a thin launcher that spawns
// this binary; the binary does all the work.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func sleep(d time.Duration) { time.Sleep(d) }

// register posts the bound port to the env until it accepts it (200) or
// reports another registration already won (409). Contract errors (400/405)
// end the program; anything else is retried with backoff.
func register(envURL string, port int) error {
	client := &http.Client{Timeout: 30 * time.Second}
	body, _ := json.Marshal(map[string]int{"port": port})
	delay := 250 * time.Millisecond
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("POST", envURL+"/register", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("content-type", "application/json")
		res, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode == 200 || res.StatusCode == 409 {
				return nil
			}
			if res.StatusCode == 400 || res.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d", res.StatusCode)
			}
		}
		sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

// waitForDone polls GET /done until it answers {"done": true}.
func waitForDone(envURL string) {
	client := &http.Client{Timeout: 30 * time.Second}
	delay := 250 * time.Millisecond
	for {
		done := false
		res, err := client.Get(envURL + "/done")
		if err == nil {
			var payload struct {
				Done bool `json:"done"`
			}
			if decErr := json.NewDecoder(res.Body).Decode(&payload); decErr == nil {
				done = payload.Done
			}
			res.Body.Close()
		}
		if done {
			return
		}
		sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

func main() {
	store := NewStore()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	// A SIGINT/SIGTERM (trial teardown) ends the forge cleanly with exit 0
	// instead of the default signal death, so the launcher reports success.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		os.Exit(0)
	}()

	envURL := os.Getenv("MAKEFUN_ENV_URL")
	// Always announce the bound port on stdout: the local harness greps the
	// server log for it, and the trial env learns it from /register.
	fmt.Printf("github-core-clone listening on http://127.0.0.1:%d\n", port)
	if envURL == "" {
		serve(ln, store) // never returns
		return
	}

	go serve(ln, store)
	if err := register(envURL, port); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "[entry] registered; waiting for the env to say done")
	waitForDone(envURL)
	os.Exit(0)
}
