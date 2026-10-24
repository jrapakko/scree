//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package lock

import (
	"log"
	"os"
	"sync"
)

var warnOnce sync.Once

func tryLock(*os.File) (held bool, err error) {
	warnOnce.Do(func() { log.Print("scree: no file locking on this OS; run one writer per index") })
	return false, nil
}

func unlock(*os.File) {}

// noFollow is 0 here; notSymlink checks first.
const noFollow = 0
