//go:build !linux

package main

import (
	"errors"
	"time"
)

// sysClock exists off-Linux only so the package builds and its unit
// tests run on developer machines; the agent itself only runs on Linux.
type sysClock struct{}

var errClockUnsupported = errors.New("setting the clock is only implemented on linux")

func (sysClock) SetSystemTime(time.Time) error { return errClockUnsupported }
func (sysClock) SetRTC(time.Time) error        { return errClockUnsupported }
