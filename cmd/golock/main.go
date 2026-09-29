// Command golock locks Go module dependencies and verifies them offline.
//
// Usage:
//
//	golock lock   [-C dir] [--cache dir]
//	golock verify [-C dir] [--cache dir] [-v]
package main

import "context"

func main() {
	run(context.Background())
}

func run(ctx context.Context) {}
