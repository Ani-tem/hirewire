package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Ani-tem/hirewire/internal/fetcher"
)

func main() {
	// Never call the network without a timeout: a hung server would hang us forever.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 10 * time.Second}

	jobs, err := fetcher.FetchGreenhouse(ctx, client, "stripe")
	if err != nil {
		log.Fatal(err)
	}

	matched := 0
	for _, j := range jobs {
		if strings.Contains(strings.ToLower(j.Title), "engineer") {
			fmt.Printf("%-55s | %-35s | %s\n", j.Title, j.Location, j.URL)
			matched++
		}
	}
	fmt.Printf("\n%d engineering roles out of %d total\n", matched, len(jobs))
}
