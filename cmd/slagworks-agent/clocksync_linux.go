package main

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// sysClock sets CLOCK_REALTIME and /dev/rtc0. Not unit-tested: both
// need CAP_SYS_TIME and the RTC needs real hardware; syncClock's logic
// is covered through the clockSetter fake.
type sysClock struct{}

func (sysClock) SetSystemTime(t time.Time) error {
	ts := unix.NsecToTimespec(t.UnixNano())
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
		return fmt.Errorf("clock_settime(CLOCK_REALTIME): %w", err)
	}
	return nil
}

// SetRTC writes t to /dev/rtc0 in UTC, the convention Ubuntu assumes
// (no LOCAL in /etc/adjtime).
func (sysClock) SetRTC(t time.Time) error {
	f, err := os.OpenFile("/dev/rtc0", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open /dev/rtc0: %w", err)
	}
	defer f.Close()
	u := t.UTC()
	rt := unix.RTCTime{
		Sec:  int32(u.Second()),
		Min:  int32(u.Minute()),
		Hour: int32(u.Hour()),
		Mday: int32(u.Day()),
		Mon:  int32(u.Month()) - 1,
		Year: int32(u.Year()) - 1900,
		Wday: int32(u.Weekday()),
		Yday: int32(u.YearDay()) - 1,
	}
	if err := unix.IoctlSetRTCTime(int(f.Fd()), &rt); err != nil {
		return fmt.Errorf("RTC_SET_TIME on /dev/rtc0: %w", err)
	}
	return nil
}
