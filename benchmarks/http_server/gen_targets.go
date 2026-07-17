package main

import (
	"fmt"
	"math/rand"
	"os"
)

const workingSetSize = 100000
const numOps = 2000000 // Total targets

func main() {
	r := rand.New(rand.NewSource(42))
	zipf := rand.NewZipf(r, 1.001, 1, uint64(workingSetSize-1))

	f, err := os.Create("targets.txt")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	for i := 0; i < numOps; i++ {
		id := zipf.Uint64()
		fmt.Fprintf(f, "GET http://localhost:8099/route/%d\n", id)
	}

	fmt.Println("Generated targets.txt")
}
