//go:build !linux

package main

import "errors"

var errUnsupported = errors.New("the agent supports Linux only")

func readCPU() (cpuTimes, error)       { return cpuTimes{}, errUnsupported }
func readMem() (float64, error)        { return 0, errUnsupported }
func readDisk(string) (float64, error) { return 0, errUnsupported }
