package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
)

const workingSetSize = 100000
const numOps = 2000000 // Total targets

func main() {
	endpoint := flag.String("endpoint", "route", "Endpoint to generate targets for (e.g. route or proxy)")
	flag.Parse()

	r := rand.New(rand.NewSource(42))
	zipf := rand.NewZipf(r, 1.001, 1, uint64(workingSetSize-1))

	filename := "targets.txt"
	if *endpoint == "proxy" {
		filename = "targets_proxy.txt"
	}

	f, err := os.Create(filename)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	for i := 0; i < numOps; i++ {
		id := zipf.Uint64()
		fmt.Fprintf(f, "GET http://localhost:8099/%s/%d\n", *endpoint, id)
	}

	fmt.Println("Generated", filename)
}
