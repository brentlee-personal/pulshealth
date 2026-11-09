package main

import (
	"math/big"
	"strconv"
	"strings"
	"time"
)

// Keep wire precision separate from PostgreSQL's existing microsecond keys.
func familySourceTime(milliseconds float64) string {
	number := strconv.FormatFloat(milliseconds, 'f', -1, 64)
	value, ok := new(big.Rat).SetString(number)
	if !ok {
		panic("non-finite validated source time")
	}
	places := 3
	if dot := strings.IndexByte(number, '.'); dot >= 0 {
		places += len(number) - dot - 1
	}
	value.Quo(value, big.NewRat(1000, 1))
	whole, remainder := new(big.Int), new(big.Int)
	whole.QuoRem(value.Num(), value.Denom(), remainder)
	if remainder.Sign() < 0 {
		whole.Sub(whole, big.NewInt(1))
		remainder.Add(remainder, value.Denom())
	}
	fraction := new(big.Rat).SetFrac(remainder, value.Denom()).FloatString(places)
	digits := strings.TrimRight(strings.TrimPrefix(fraction, "0."), "0")
	stamp := time.Unix(whole.Int64(), 0).UTC().Format("2006-01-02T15:04:05")
	if digits != "" {
		stamp += "." + digits
	}
	return stamp + "Z"
}
