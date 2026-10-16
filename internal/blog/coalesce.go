package blog

import "sync"

type rebuildStatus struct{ running, dirty bool }

var pendingRebuilds = struct {
	sync.Mutex
	jobs map[string]*rebuildStatus
}{jobs: make(map[string]*rebuildStatus)}

// Claim coalesces queued work by account/post. Work arriving during a build
// marks it dirty so the worker performs one final pass.
func Claim(key string) bool {
	pendingRebuilds.Lock()
	defer pendingRebuilds.Unlock()
	if status := pendingRebuilds.jobs[key]; status != nil {
		if status.running {
			status.dirty = true
		}
		return false
	}
	pendingRebuilds.jobs[key] = &rebuildStatus{}
	return true
}

func Cancel(key string) {
	pendingRebuilds.Lock()
	delete(pendingRebuilds.jobs, key)
	pendingRebuilds.Unlock()
}

func Start(key string) {
	pendingRebuilds.Lock()
	status := pendingRebuilds.jobs[key]
	if status == nil {
		status = &rebuildStatus{}
		pendingRebuilds.jobs[key] = status
	}
	status.running = true
	pendingRebuilds.Unlock()
}

func Finish(key string, success bool) bool {
	pendingRebuilds.Lock()
	defer pendingRebuilds.Unlock()
	status := pendingRebuilds.jobs[key]
	if !success || status == nil || !status.dirty {
		delete(pendingRebuilds.jobs, key)
		return false
	}
	status.dirty = false
	return true
}
