package inbound

import "time"

type timer struct {
	t *time.Timer
	c <-chan time.Time
}

func newTimer(seconds int) *timer {
	t := time.NewTimer(time.Duration(seconds) * time.Second)
	return &timer{t: t, c: t.C}
}

func (t *timer) stop() {
	if !t.t.Stop() {
		select {
		case <-t.c:
		default:
		}
	}
}
