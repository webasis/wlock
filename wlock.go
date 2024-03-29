package wlock

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/webasis/wrpc"
	"github.com/webasis/wrpc/wret"
)

type Locker struct {
	Id     string
	Secret string // update while new/free
	Token  string // update while lock/unlock
	Locked bool

	LastHold  time.Time // for auto free
	LastTouch time.Time // for auto unlock
}

type Status struct {
	Total  int `json:"total"`
	Locked int `json:"locked"`
}

type LMFunc func(lm *LockerManager)

type LockerManager struct {
	C chan LMFunc // never close

	Lockers map[string]*Locker // map[id]

	// read-only
	NextId             func() string
	NextSecret         func() string
	AutoFreeInterval   time.Duration
	AutoUnlockInterval time.Duration
	GCInterval         time.Duration
}

func DefaultNextId() func() string {
	nextId := 1
	mu := new(sync.Mutex)
	return func() string {
		mu.Lock()
		defer mu.Unlock()

		id := fmt.Sprint(nextId)
		nextId++
		return id
	}
}

func DefaultNextSecret() string {
	return uuid.New().String()
}

func New() *LockerManager {
	const DEFAULT_SIZE = 1
	lm := &LockerManager{
		C:       make(chan LMFunc, DEFAULT_SIZE),
		Lockers: make(map[string]*Locker),

		NextId:             DefaultNextId(),
		NextSecret:         DefaultNextSecret,
		AutoFreeInterval:   time.Second * 300,
		AutoUnlockInterval: time.Second * 60,
		GCInterval:         time.Second * 600,
	}

	go lm.loop()
	return lm
}

func (lm *LockerManager) loop() {
	// gc
	go func() {
		for {
			time.Sleep(lm.GCInterval)
			lm.C <- func(lm *LockerManager) {
				lm.GC()
			}
		}
	}()

	for fn := range lm.C {
		fn(lm)
	}
}

func (lm *LockerManager) gc(l *Locker) {
	if time.Now().Sub(l.LastHold) > lm.AutoFreeInterval {
		lm.Free(l.Id, l.Secret)
		return
	}
	if l.Locked && time.Now().Sub(l.LastTouch) > lm.AutoUnlockInterval {
		lm.Unlock(l.Id, l.Token)
		return
	}
}

func (lm *LockerManager) GC() {
	for _, l := range lm.Lockers {
		lm.gc(l)
	}
}

func (lm *LockerManager) New() (id string, secret string) {
	id = lm.NextId()
	secret = lm.NextSecret()
	now := time.Now()
	l := &Locker{
		Id:        id,
		Secret:    secret,
		Token:     "",
		Locked:    false,
		LastHold:  now,
		LastTouch: now,
	}

	lm.Lockers[id] = l
	return id, secret
}

func (lm *LockerManager) Free(id string, secret string) bool {
	l := lm.Lockers[id]
	if l == nil {
		return false
	}

	if l.Secret != secret {
		return false
	}

	delete(lm.Lockers, id)
	return true
}

func (lm *LockerManager) Hold(id string, secret string) bool {
	l := lm.Lockers[id]
	if l == nil {
		return false
	}

	if l.Secret != secret {
		return false
	}

	l.LastHold = time.Now()
	return true
}

func (lm *LockerManager) Lock(id string) (token string) {
	l := lm.Lockers[id]
	if l == nil {
		return ""
	}

	lm.gc(l)

	if l.Locked == true {
		return ""
	}
	token = lm.NextSecret()
	l.Token = token
	l.Locked = true
	l.LastTouch = time.Now()
	return token
}

func (lm *LockerManager) Unlock(id string, token string) bool {
	l := lm.Lockers[id]
	if l == nil {
		return false
	}

	if l.Locked == false {
		return false
	}

	if l.Token != token {
		return false
	}

	l.Locked = false
	l.Token = ""
	return true
}

func (lm *LockerManager) Touch(id string, token string) bool {
	l := lm.Lockers[id]
	if l == nil {
		return false
	}

	if l.Locked == false {
		return false
	}

	if l.Token != token {
		return false
	}

	l.LastTouch = time.Now()
	return true
}

func (lm *LockerManager) Status() Status {
	s := Status{
		Total:  len(lm.Lockers),
		Locked: 0,
	}

	for _, l := range lm.Lockers {
		if l.Locked {
			s.Locked++
		}
	}

	return s
}

func (lm *LockerManager) Sync(fn func()) {
	done := make(chan bool, 1)
	lm.C <- func(lm *LockerManager) {
		defer close(done)
		fn()
	}
	<-done
}

func Enable(rpc *wrpc.Server, lm *LockerManager) {
	rpc.HandleFunc("wlock/new", func(r wrpc.Req) wrpc.Resp {
		var id, secret string
		lm.Sync(func() {
			id, secret = lm.New()
		})
		return wret.OK(id, secret)
	})
	rpc.HandleFunc("wlock/free", func(r wrpc.Req) wrpc.Resp {
		if len(r.Args) != 2 {
			return wret.Error("args")
		}

		id := r.Args[0]
		secret := r.Args[1]
		var ok bool
		lm.Sync(func() {
			ok = lm.Free(id, secret)
		})

		if ok {
			return wret.OK()
		} else {
			return wret.Error()
		}
	})
	rpc.HandleFunc("wlock/renew", func(r wrpc.Req) wrpc.Resp {
		if len(r.Args) != 2 {
			return wret.Error("args")
		}

		id := r.Args[0]
		secret := r.Args[1]
		var ok bool
		lm.Sync(func() {
			ok = lm.Hold(id, secret)
		})

		if ok {
			return wret.OK()
		} else {
			return wret.Error()
		}
	})
	rpc.HandleFunc("wlock/status", func(r wrpc.Req) wrpc.Resp {
		if len(r.Args) != 1 {
			return wret.Error("args")
		}

		var status string
		id := r.Args[0]
		lm.Sync(func() {
			l := lm.Lockers[id]
			if l != nil {
				lm.gc(l)
				l = lm.Lockers[id]
			}

			if l == nil {
				status = "not_found"
				return
			}
			if l.Locked {
				status = "locked"
			} else {
				status = "unlocked"
			}
		})

		return wret.OK(status)
	})
	rpc.HandleFunc("wlock/lock", func(r wrpc.Req) wrpc.Resp {
		if len(r.Args) != 1 {
			return wret.Error("args")
		}

		id := r.Args[0]
		var token string
		lm.Sync(func() {
			token = lm.Lock(id)
		})

		if len(token) > 0 {
			return wret.OK(token)
		} else {
			return wret.Error()
		}
	})
	rpc.HandleFunc("wlock/unlock", func(r wrpc.Req) wrpc.Resp {
		if len(r.Args) != 2 {
			return wret.Error("args")
		}

		id := r.Args[0]
		token := r.Args[1]
		var ok bool
		lm.Sync(func() {
			ok = lm.Unlock(id, token)
		})

		if ok {
			return wret.OK()
		} else {
			return wret.Error()
		}
	})

	rpc.HandleFunc("wlock/touch", func(r wrpc.Req) wrpc.Resp {
		if len(r.Args) != 2 {
			return wret.Error("args")
		}

		id := r.Args[0]
		token := r.Args[1]
		var ok bool
		lm.Sync(func() {
			ok = lm.Touch(id, token)
		})

		if ok {
			return wret.OK()
		} else {
			return wret.Error()
		}
	})

	rpc.HandleFunc("wlock/admin/status", func(r wrpc.Req) wrpc.Resp {
		var s Status
		lm.Sync(func() {
			s = lm.Status()
		})

		raw, err := json.Marshal(s)
		if err != nil {
			return wret.IError(err.Error())
		}

		return wret.OK(string(raw))
	})
}
