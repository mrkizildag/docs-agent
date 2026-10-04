package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"
)

const requestTimeout = 2 * time.Second

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: healthcheck <url>")
		os.Exit(2)
	}
	if err := run(context.Background(), os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, url string) error {
	return check(ctx, &http.Client{Timeout: requestTimeout}, url)
}

func check(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // the URL is the compose health check's own argument
	if err != nil {
		return fmt.Errorf("build request %s: %w", url, err)
	}
	resp, err := client.Do(req) //nolint:gosec // see above
	if err != nil {
		return fmt.Errorf("get %s: %w", url, err)
	}
	if err := resp.Body.Close(); err != nil {
		return fmt.Errorf("close response from %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}
	return nil
}
