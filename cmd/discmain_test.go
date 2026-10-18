package main

import (
	"fmt"
	"os"

	"github.com/ormasoftchile/a4c/pkg/a4c"
)

func main() {
	endpoint, _, ok := a4c.DiscoverOpenCodeService()
	fmt.Printf("discovered ok=%v endpoint=%q\n", ok, endpoint)
	if reg, _ := os.ReadFile("/Users/cormazab/.local/state/opencode/service.json"); reg != nil {
		fmt.Println("(registration exists on disk)")
	}
}
