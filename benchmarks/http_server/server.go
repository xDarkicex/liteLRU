package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/maypok86/otter"
	"github.com/xDarkicex/liteLRU"
)

var cacheType = flag.String("cache", "none", "Cache to use: none, litelru, otter")

func main() {
	flag.Parse()

	const capacity = 75000 // 75% of 100K working set

	var lite *liteLRU.LRUCache
	var otterCache otter.Cache[string, string]

	if *cacheType == "litelru" {
		lite = liteLRU.NewLRUCache(capacity, 5)
		fmt.Println("Using liteLRU cache")
	} else if *cacheType == "otter" {
		var err error
		otterCache, err = otter.MustBuilder[string, string](capacity).Build()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("Using Otter v2 cache")
	} else {
		fmt.Println("Using NO cache (origin only)")
	}

	http.HandleFunc("/route/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/route/")
		if id == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}

		if *cacheType == "litelru" {
			if _, _, ok := lite.Get("GET", id); ok {
				// Cache hit
				w.Write([]byte("ok"))
				return
			}
			// Cache miss - compute and add
			lite.Add("GET", id, nil, nil)
			w.Write([]byte("ok"))
			return
		}

		if *cacheType == "otter" {
			if _, ok := otterCache.Get(id); ok {
				// Cache hit
				w.Write([]byte("ok"))
				return
			}
			// Cache miss - compute and set
			otterCache.Set(id, "ok")
			w.Write([]byte("ok"))
			return
		}

		// No cache
		w.Write([]byte("ok"))
	})

	fmt.Println("Listening on :8099...")
	log.Fatal(http.ListenAndServe(":8099", nil))
}
