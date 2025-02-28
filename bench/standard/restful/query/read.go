package main

import "github.com/i-Things/driver-go/v3/bench/standard/executor"

func main() {
	test := executor.NewTDTest(executor.DefaultRestfulDriverName, executor.DefaultRestfulDSN)

	test.Clean()
	tableName := test.PrepareRead(1000, 100)
	test.BenchmarkRead("select * from " + tableName)
}
