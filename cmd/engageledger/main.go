// Command engageledger reads an engagement journal and reports what it adds
// up to. verify walks the chain, the canonical form of every line, the
// approval evidence and the recorded deadlines, and names the first line it
// refuses; fold prints the milestones, the price, the cap and the evidence
// strength of every change order; clock prints the acceptance windows whose
// clock is running, each deadline in UTC and in the zone the contract counts
// business days in. Every command writes canonical JSON over integers and
// recorded instants alone, so one journal gives the same bytes on every run
// and every machine.
package main

import "os"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
