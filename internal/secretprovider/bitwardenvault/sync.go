package bitwardenvault

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func (p *Provider) startPeriodicSync(host, port string) {
	syncURL := fmt.Sprintf("http://%s:%s/sync", host, port)
	fmt.Printf("Starting periodic sync every %s targeting %s\n", p.cfg.SyncInterval, syncURL)
	ticker := time.NewTicker(p.cfg.SyncInterval)
	defer ticker.Stop()

	for range ticker.C {
		fmt.Println("Periodic sync triggered...")
		resp, err := http.Post(syncURL, "application/json", nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Periodic sync failed: %v", err)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Periodic sync failed with status code: %d and could not read body: %v\n", resp.StatusCode, err)
			} else {
				fmt.Fprintf(os.Stderr, "Periodic sync failed with status code: %d, body: %s\n", resp.StatusCode, string(body))
			}
		}
		_ = resp.Body.Close()
	}
}
