package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/LJAM96/replx/internal/inventory"
)

func main() {
	base := os.Getenv("REPLX_EDGE_PMS_URL")
	token := os.Getenv("REPLX_EDGE_PMS_TOKEN")
	pageSize, err := envInt("REPLX_EDGE_INVENTORY_PAGE_SIZE", 200)
	if err != nil {
		fail(err)
	}
	workers, err := envInt("REPLX_EDGE_INVENTORY_WORKERS", 4)
	if err != nil {
		fail(err)
	}
	timeout, err := envInt("REPLX_EDGE_INVENTORY_REQUEST_TIMEOUT_SECONDS", 15)
	if err != nil {
		fail(err)
	}
	client, err := inventory.New(inventory.Config{BaseURL: base, Token: token, PageSize: pageSize, Workers: workers, RequestTimeout: time.Duration(timeout) * time.Second})
	if err != nil {
		fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	report := client.Inventory(ctx)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		fail(fmt.Errorf("write report: %w", err))
	}
	if report.HasIncompleteCounts() {
		os.Exit(1)
	}
}
func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, e := strconv.Atoi(v)
	if e != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return n, nil
}
func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(2) }
