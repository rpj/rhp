package main

import (
	"fmt"
)

func RhpPlugin (payload interface{}) (interface{}, error) {
	fmt.Printf("rpjios plugin main('%s')\n", payload)
	return payload, nil
}
