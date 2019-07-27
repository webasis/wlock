package wlock

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"math/rand"
)

func TestLocker(t *testing.T) {
	lm := New()
	go func() {
		for {
			lm.C <- func(lm *LockerManager) {
				s := lm.Status()
				fmt.Printf("status: (%d/%d)\n", s.Locked, s.Total)
			}
			time.Sleep(time.Millisecond * 3)
		}
	}()

	// clients
	wg := new(sync.WaitGroup)

	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			sleep_randomly := func() {
				time.Sleep(time.Millisecond * time.Duration(rand.Int()%10))
			}

			synch := make(chan bool, 1)
			call := func(fn func()) {
				lm.C <- func(lm *LockerManager) {
					fn()
					synch <- true
				}
				<-synch
				sleep_randomly()
			}

			id := ""
			secret := ""
			token := ""

			call(func() {
				id, secret = lm.New()
			})
			call(func() {
				token = lm.Lock(id)
			})
			call(func() {
				lm.Unlock(id, token)
			})
			call(func() {
				lm.Free(id, secret)
			})
		}()
	}
	wg.Wait()
}
