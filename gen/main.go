package main

import (
	"fmt"
	"github.com/lotus-web3/ribs/bsst"
	"os"

	gen "github.com/whyrusleeping/cbor-gen"

	"github.com/lotus-web3/ribs/jbob"
)

func main() {
	err := gen.WriteTupleEncodersToFile("./ribs/jbob/cbor_gen.go", "jbob", jbob.Head{})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	err = gen.WriteMapEncodersToFile("./ribs/bsst/cbor_gen.go", "bsst", bsst.BSSTHeader{})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
