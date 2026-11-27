// Command airview-healthcheck probes a running AirView server and exits 0
// when it answers, 1 otherwise. It exists for container images without a
// shell, curl or wget (Dockerfile HEALTHCHECK in a distroless image):
//
//	airview-healthcheck [-url http://127.0.0.1:8080/healthz] [-timeout 5s]
//
// Without -url the address comes from $AIRVIEW_HEALTHCHECK_URL, else
// http://127.0.0.1:$AIRVIEW_HTTP_PORT/healthz (port 80 when unset), so
// the probe follows the same AIRVIEW_HTTP_PORT override as the server.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stderr))
}

// defaultURL derives the probe URL from the environment.
func defaultURL(getenv func(string) string) string {
	if u := strings.TrimSpace(getenv("AIRVIEW_HEALTHCHECK_URL")); u != "" {
		return u
	}
	port := 80
	if p, err := strconv.Atoi(strings.TrimSpace(getenv("AIRVIEW_HTTP_PORT"))); err == nil && p > 0 && p <= 65535 {
		port = p
	}
	return fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
}

func run(args []string, getenv func(string) string, stderr io.Writer) int {
	fs := flag.NewFlagSet("airview-healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "URL to probe (default: $AIRVIEW_HEALTHCHECK_URL, else http://127.0.0.1:$AIRVIEW_HTTP_PORT/healthz)")
	timeout := fs.Duration("timeout", 5*time.Second, "give up after this long")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *url == "" {
		*url = defaultURL(getenv)
	}
	if err := probe(*url, *timeout); err != nil {
		fmt.Fprintln(stderr, "airview-healthcheck:", err)
		return 1
	}
	return 0
}

// probe GETs url and requires a 2xx answer within timeout.
func probe(url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "airview-healthcheck")
	// No proxies: the probe always targets the local server.
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return nil
}
