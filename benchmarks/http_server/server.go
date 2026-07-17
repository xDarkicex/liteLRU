package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maypok86/otter"
	"github.com/xDarkicex/liteLRU"
)


type RouteResult struct {
	Handler func()
	Params  []liteLRU.Param
}

var cacheType = flag.String("cache", "none", "Cache to use: none, litelru, otter")

type ComplexPayload struct {
	ID          int                      `json:"id"`
	Title       string                   `json:"title"`
	Description string                   `json:"description"`
	Timestamp   time.Time                `json:"timestamp"`
	Tags        []string                 `json:"tags"`
	Metadata    map[string]interface{}   `json:"metadata"`
	Author      Author                   `json:"author"`
}

type Author struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Verified bool   `json:"verified"`
}

var payloads [20]ComplexPayload

func init() {
	for i := 0; i < 20; i++ {
		payloads[i] = ComplexPayload{
			ID:          i,
			Title:       fmt.Sprintf("Synthetic Title %d", i),
			Description: fmt.Sprintf("This is a dynamically generated complex description for payload number %d meant to take up some space and CPU time.", i),
			Timestamp:   time.Now().Add(time.Duration(i) * time.Hour),
			Tags:        []string{"golang", "performance", "caching", "systems", "benchmark"},
			Metadata: map[string]interface{}{
				"views":        i * 1000,
				"active":       i%2 == 0,
				"score":        99.9 + float64(i),
				"coefficients": []float64{1.1, 2.2, 3.3, 4.4, 5.5},
			},
			Author: Author{
				Name:     fmt.Sprintf("Author %d", i),
				Email:    fmt.Sprintf("author%d@example.com", i),
				Verified: true,
			},
		}
	}
}

func main() {
	flag.Parse()

	const capacity = 75000 // 75% of 100K working set

	var lite *liteLRU.LRUCache
	var otterCache otter.Cache[string, string]
	var otterRouterCache otter.Cache[string, RouteResult]

	if *cacheType == "litelru" {
		lite = liteLRU.NewLRUCache(capacity, 5)
		fmt.Println("Using liteLRU cache")
	} else if *cacheType == "otter" {
		var err error
		otterCache, err = otter.MustBuilder[string, string](capacity).Build()
		if err != nil {
			log.Fatal(err)
		}
		
		var err2 error
		otterRouterCache, err2 = otter.MustBuilder[string, RouteResult](capacity).Build()
		if err2 != nil {
			log.Fatal(err2)
		}
		fmt.Println("Using Otter v2 cache")
	} else {
		fmt.Println("Using NO cache (origin only)")
	}

	http.HandleFunc("/route/", func(w http.ResponseWriter, r *http.Request) {
		idStr := strings.TrimPrefix(r.URL.Path, "/route/")
		if idStr == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}

		idNum, err := strconv.Atoi(idStr)
		if err != nil {
			idNum = 0
		}
		
		payload := payloads[idNum%20]

		if *cacheType == "litelru" {
			var pbuf [1]liteLRU.Param
			if _, params, ok := lite.Get("GET", idStr, pbuf[:0]); ok && len(params) > 0 {
				// Cache hit
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(params[0].Value))
				return
			}
			// Cache miss - compute (marshal) and add
			b, _ := json.Marshal(payload)
			lite.Add("GET", idStr, nil, []liteLRU.Param{{Key: "res", Value: string(b)}})
			
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
			return
		}

		if *cacheType == "otter" {
			if cachedRes, ok := otterCache.Get(idStr); ok {
				// Cache hit
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(cachedRes))
				return
			}
			// Cache miss - compute (marshal) and set
			b, _ := json.Marshal(payload)
			otterCache.Set(idStr, string(b))
			
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
			return
		}

		// No cache - marshal every time
		b, _ := json.Marshal(payload)
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	})

	http.HandleFunc("/proxy/", func(w http.ResponseWriter, r *http.Request) {
		idStr := strings.TrimPrefix(r.URL.Path, "/proxy/")
		if idStr == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}

		idNum, err := strconv.Atoi(idStr)
		if err != nil {
			idNum = 0
		}
		
		payload := payloads[idNum%20]

		if *cacheType == "litelru" {
			var pbuf [1]liteLRU.Param
			if _, params, ok := lite.Get("GET", "/proxy/"+idStr, pbuf[:0]); ok && len(params) > 0 {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(params[0].Value))
				return
			}
			// Simulate upstream network latency
			time.Sleep(50*time.Millisecond + time.Duration(idNum%20)*time.Millisecond)
			b, _ := json.Marshal(payload)
			lite.Add("GET", "/proxy/"+idStr, nil, []liteLRU.Param{{Key: "res", Value: string(b)}})
			
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
			return
		}

		if *cacheType == "otter" {
			if cachedRes, ok := otterCache.Get("/proxy/" + idStr); ok {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(cachedRes))
				return
			}
			// Simulate upstream network latency
			time.Sleep(50*time.Millisecond + time.Duration(idNum%20)*time.Millisecond)
			b, _ := json.Marshal(payload)
			otterCache.Set("/proxy/"+idStr, string(b))
			
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
			return
		}

		// Simulate upstream network latency
		time.Sleep(50*time.Millisecond + time.Duration(idNum%20)*time.Millisecond)
		b, _ := json.Marshal(payload)
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	})

	
	http.HandleFunc("/api/user/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		if *cacheType == "litelru" {
			var pbuf [4]liteLRU.Param
			if handler, params, ok := lite.Get("GET", path, pbuf[:0]); ok {
				if handler != nil {
					handler()
				}
				w.Header().Set("Content-Type", "text/plain")
				if len(params) > 0 {
					w.Write([]byte(params[0].Value))
				}
				return
			}
			
			parts := strings.Split(path, "/")
			if len(parts) >= 5 && parts[4] == "profile" {
				idStr := parts[3]
				time.Sleep(1 * time.Millisecond) // Simulate route parsing & middleware overhead
				
				handler := func() {}
				params := []liteLRU.Param{{Key: "id", Value: idStr}}
				lite.Add("GET", path, handler, params)
				
				handler()
				w.Header().Set("Content-Type", "text/plain")
				w.Write([]byte(idStr))
				return
			}
			http.NotFound(w, r)
			return
		}

		if *cacheType == "otter" {
			if res, ok := otterRouterCache.Get(path); ok {
				if res.Handler != nil {
					res.Handler()
				}
				w.Header().Set("Content-Type", "text/plain")
				if len(res.Params) > 0 {
					w.Write([]byte(res.Params[0].Value))
				}
				return
			}
			
			parts := strings.Split(path, "/")
			if len(parts) >= 5 && parts[4] == "profile" {
				idStr := parts[3]
				time.Sleep(1 * time.Millisecond) // Simulate route parsing & middleware overhead
				
				res := RouteResult{
					Handler: func() {},
					Params:  []liteLRU.Param{{Key: "id", Value: idStr}},
				}
				otterRouterCache.Set(path, res)
				
				res.Handler()
				w.Header().Set("Content-Type", "text/plain")
				w.Write([]byte(idStr))
				return
			}
			http.NotFound(w, r)
			return
		}

		parts := strings.Split(path, "/")
		if len(parts) >= 5 && parts[4] == "profile" {
			idStr := parts[3]
			time.Sleep(1 * time.Millisecond) // Simulate route parsing & middleware overhead
			
			handler := func() {}
			handler()
			
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(idStr))
			return
		}
		http.NotFound(w, r)
	})

	fmt.Println("Listening on :8099...")
	log.Fatal(http.ListenAndServe(":8099", nil))
}
