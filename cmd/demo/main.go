// Command demo walks through the core behaviours of the matching engine:
// resting orders, price priority, sweeping several levels, and partial fills.
package main

import (
	"fmt"

	"github.com/liliang198909/matching-engine/engine"
)

func main() {
	e := engine.NewEngine()

	fmt.Println("== 1. Sell orders submitted out of order — the book sorts by price ==")
	e.SubmitOrder(5, 101, false)
	e.SubmitOrder(8, 102, false)
	e.SubmitOrder(3, 100, false)
	printBook(e)

	fmt.Println("\n== 2. A buy order takes the 100 level (price priority) ==")
	printTrades(e.SubmitOrder(3, 100, true))
	printBook(e)

	fmt.Println("\n== 3. A large buy order sweeps the 101 and 102 levels ==")
	printTrades(e.SubmitOrder(9, 102, true))
	printBook(e)

	fmt.Println("\n== 4. A buy order fills partially; 2 lots rest at 102 ==")
	printTrades(e.SubmitOrder(6, 102, true))
	printBook(e)

	fmt.Printf("\nTotal trades: %d\n", len(e.Trades()))
}

func printTrades(trades []engine.Trade) {
	if len(trades) == 0 {
		fmt.Println("  no fill — order rested on the book")
		return
	}
	for _, tr := range trades {
		fmt.Printf("  filled %v @ %v (bid #%d vs ask #%d)\n",
			tr.Size, tr.Price, tr.BidOrder, tr.AskOrder)
	}
}

func printBook(e *engine.Engine) {
	bids, asks := e.Depth(5)

	fmt.Println("  Asks (ascending):")
	if len(asks) == 0 {
		fmt.Println("    (empty)")
	}
	for _, l := range asks {
		fmt.Printf("    %-8v x%v\n", l.Price, l.Volume)
	}

	fmt.Println("  Bids (descending):")
	if len(bids) == 0 {
		fmt.Println("    (empty)")
	}
	for _, l := range bids {
		fmt.Printf("    %-8v x%v\n", l.Price, l.Volume)
	}
}
